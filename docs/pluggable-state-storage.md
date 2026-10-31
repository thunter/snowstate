# Terraform Pluggable State Storage (`TF_ENABLE_PLUGGABLE_STATE_STORAGE`): Investigation

| | |
|---|---|
| **Author** | Todd Hunter (prepared with Claude) |
| **Date** | 31 October 2026 |
| **Status** | Findings + recommendation |
| **Related** | `docs/initial-review.md`, `SPEC.md` |

---

## 1. Summary

Terraform has an **experimental feature called Pluggable State Storage (PSS)**. It lets a **provider** implement state storage, through a new `state_store` block that replaces `backend`. This is the "backends as plugins" capability that the initial review said didn't exist. It does exist now, but only as an experiment:

- It **only works in Terraform alpha builds** (or a source build with experiments compiled in). The current stable releases (v1.16.7, v1.17.1) refuse it with an error. The most recent alpha that has it is **v1.17.0-alpha20260827**. Terraform `main` is now `1.18.0-dev`, and no 1.18 alpha has been published yet.
- Provider-side support shipped in **terraform-plugin-framework v1.18.0 (27 Feb 2026)**. It's marked *"experimental and offered without compatibility promises until support for `state_store` in Terraform core is generally available."*
- **No GA date has been announced.** The PSS experiment isn't listed in the `1.18.0 (Unreleased)` changelog's experiments section. That doesn't mean it's gone (the code is still on `main`), but there's no public signal that it's about to stabilise.
- **OpenTofu doesn't have it.** OpenTofu's backend-plugins work is still at the RFC stage.

**What it means for us:** PSS is the right *long-term* shape for a Snowflake backend. A `snowstate` provider would talk to Snowflake directly from inside Terraform, with no HTTP server, native workspaces, and Snowflake auth configured in HCL. But it **can't be used in production today**. So keep building the HTTP backend from `SPEC.md` (it works on stable Terraform and OpenTofu now), and structure the code so a PSS front-end can sit on the same Snowflake storage layer later. I recommend adding it as an **optional, experimental milestone** (§7).

---

## 2. How it's enabled

```bash
# Either:
export TF_ENABLE_PLUGGABLE_STATE_STORAGE=1        # any non-empty value
terraform init
# or:
terraform init -enable-pluggable-state-storage-experiment
```

From the source (`internal/command/arguments/init.go`, `internal/command/init_run.go`):

- The flag/env var is read **on `init` only**. Once a working directory is initialised with a state store, later `plan`/`apply` commands use it.
- If the binary wasn't built with experiments allowed, `init` fails with: *"Cannot use -enable-pluggable-state-storage-experiment flag without experiments enabled"*. A config containing a `state_store` block is also rejected.
- Experiments are allowed only in **alpha releases**, or in a binary built with `go install -ldflags="-X 'main.experimentsAllowed=yes'"`. A custom build is possible for a spike, but it isn't a supported Terraform.
- There's also a new `init` flag, `-state-provider-lock-file` (only with the experiment and `-input=false`), which points at the lock file used to trust the state-storage provider.

## 3. Configuration shape

```hcl
terraform {
  required_providers {
    snowstate = {
      source  = "example/snowstate"
      version = "~> 0.1"
    }
  }

  state_store "snowstate_snowflake" {      # <provider>_<store name>
    provider "snowstate" {                 # provider config lives INSIDE the block
      account   = "myorg-myaccount"
      user      = "TF_STATE_SVC"
      role      = "SNOWSTATE_SVC"
      warehouse = "SNOWSTATE_WH"
      # auth via key pair / PAT, typically from env
    }

    database = "TF_STATE"
    schema   = "BACKEND"
    prefix   = "platform/network"          # store-specific attributes
  }
}
```

Rules enforced by core (`internal/configs/state_store.go`):

- Exactly one nested `provider` block. **No aliases.**
- The provider **must** be in `required_providers`, and **must be in `.terraform.lock.hcl`** at a version matching the constraints. Otherwise you get "Inconsistent dependency lock file".
- A state store's schema may not define anything called `provider`.
- The provider must speak **plugin protocol v6** (the RPCs only exist in `tfplugin6.proto`, protocol 6.11).

## 4. The protocol (what a provider implements)

Eight new RPCs in `tfplugin6.proto`:

| RPC | Purpose | Notes for a Snowflake implementation |
|---|---|---|
| `ValidateStateStoreConfig` | Offline config validation | No network access. |
| `ConfigureStateStore` | Connect, and negotiate the **chunk size** | Core proposes **8 MB**. The provider may choose up to **128 MB**. |
| `ReadStateBytes` | **Streams** the state for a `state_id` in chunks | Return empty (no error) when the state doesn't exist. |
| `WriteStateBytes` | **Streams** the state into the store | The framework reassembles the chunks and hands you `[]byte`. |
| `LockState` | Lock a `state_id`, return a provider-generated `lock_id` | The request carries only `state_id` and `operation`. Returning `""` means "locking not supported". |
| `UnlockState` | Unlock with the `lock_id` | Unknown or mismatched ID must return an error diagnostic. `force-unlock <id>` comes through this same path. |
| `GetStates` | List all state IDs (= **workspaces**) | Real workspace support, which the `http` backend lacks. |
| `DeleteState` | Delete a state ID | Used by `terraform workspace delete`. |

Framework Go interface (`terraform-plugin-framework/statestore`, v1.18+):

```go
type StateStore interface {
    Metadata(ctx, MetadataRequest, *MetadataResponse)
    Schema(ctx, SchemaRequest, *SchemaResponse)
    Initialize(ctx, InitializeRequest, *InitializeResponse)   // runs once, after provider Configure
    GetStates(ctx, GetStatesRequest, *GetStatesResponse)       // -> StateIDs []string
    DeleteState(ctx, DeleteStateRequest, *DeleteStateResponse) // StateID
    Lock(ctx, LockRequest, *LockResponse)                      // StateID, Operation -> LockID
    Unlock(ctx, UnlockRequest, *UnlockResponse)                // StateID, LockID
    Read(ctx, ReadRequest, *ReadResponse)                      // StateID -> StateBytes
    Write(ctx, WriteRequest, *WriteResponse)                   // StateID, StateBytes
}
// optional: StateStoreWithConfigure, StateStoreWithConfigValidators, StateStoreWithValidateConfig
// provider registers via provider.ProviderWithStateStores
```

The framework also provides a `statestore.LockInfo` helper (ID, Operation, Who, Created) and a ready-made `WorkspaceAlreadyLockedDiagnostic`.

### Semantics that matter for the design

1. **`Write` doesn't carry the lock ID.** The provider has to remember the lock it issued in-process (Lock and Write happen in the same provider process during one command), and do a **conditional write in Snowflake**: write only if the `LOCKS` row still holds our ID. This maps cleanly onto the transaction already specified in `SPEC.md` §6.4.
2. **Core always reads before writing**, including for brand-new workspaces.
3. **The store must be strongly consistent**, so a lock is immediately visible to everyone. A hybrid-table primary-key insert (SPEC §6.5) meets this. A standard-table approach is as risky here as it is for the HTTP design.
4. **Lock metadata is thin.** Core sends only `operation`, so the provider generates `Who`/`Created` itself. Conflicts come back as diagnostics, not the structured lock info the `http` backend returns.
5. **Chunking is handled for you.** With an 8 MB default and up to 128 MB per chunk, large states aren't a protocol concern. Snowflake storage limits (SPEC §6.3) still apply.

## 5. Tooling around it

- **Migration:** there's a new **`terraform state migrate`** command. It reads a `*.tfmigrate.hcl` file declaring the `from` backend or state store, and migrates to the `state_store` in the root module, with separate source/destination lock-file flags and `-force-copy` for CI. (Community providers note that before this existed, people used `state pull`/`state push`.)
- **Testing:** `terraform-plugin-testing` has a **state store mode**. `StateStore: true` exercises init, read/write and workspaces, and `VerifyStateStoreLock: true` checks lock, lock conflicts and release during `apply`. Tests typically skip unless Terraform ≥ 1.15 *and* a prerelease.
- **Core coverage:** Terraform's own e2e suite (`internal/command/e2etest/pluggable_state_store_test.go`) covers workspace commands, state commands, `output`, `show`, provider commands and state-migrate upgrades.
- **Prior art:** a community provider stores state in OCI registries (`vmvarela/oras`, Sep 2026). It only works on 1.17 alpha builds, and its tracking issue confirms that **beta and stable builds disable the feature**.

## 6. Gaps and risks

| Gap / risk | Impact |
|---|---|
| **Alpha-only.** Stable Terraform refuses it. | Can't be adopted for real infrastructure until GA. Running alpha Terraform in production isn't sensible. |
| **No compatibility promise** (core, protocol and framework) | RPCs or semantics can change between alphas, so expect rework. |
| **No GA timeline**, and it isn't in the 1.18 changelog's experiments list | We can't plan around a date. |
| **`terraform_remote_state` only accepts `backend`** (checked in `internal/builtin/providers/terraform/data_source_state.go`) | Cross-stack reads would need our own data source (e.g. `data "snowstate_outputs"`), which is easy to add in the same provider. |
| **Not in OpenTofu** | If OpenTofu matters, the HTTP backend stays necessary regardless. |
| **Provider must be in the lock file** | Adds a provider-distribution step: a registry, a private registry, or a filesystem mirror. |
| **Credentials live in the provider block / env of every run** | The same as the sidecar mode in SPEC, so it's no worse. |

Upsides compared with the HTTP backend in `SPEC.md`:

- No server, no TLS, no API tokens.
- Native workspaces.
- Locking errors surface as normal Terraform diagnostics.
- The Snowflake auth model is configured directly.
- Distribution is a normal provider.

For reference, the official **Snowflake provider** (`snowflakedb/snowflake`) is already on plugin-framework v1.19 and protocol v6 (muxed). So it *could* technically host a state store in future, but that would be Snowflake's decision. Building our own small provider doesn't depend on them.

## 7. Recommendation

1. **Don't change direction.** Build the HTTP backend per `SPEC.md`. It's the only option that works with stable Terraform and OpenTofu today.
2. **Make the storage layer reusable.** SPEC already separates `service` → `store`. Make the Snowflake store and service packages importable (`pkg/` rather than `internal/`, or a Go module boundary), so a provider can link the same code.
3. **Add an optional experimental milestone (M7): `terraform-provider-snowstate`** with:
   - a state store `snowstate_snowflake` implementing the 8 methods over the shared store (workspaces = state keys under a configured prefix, locks = hybrid `LOCKS` rows, `Write` = the conditional write in SPEC §6.4 using the in-process lock ID);
   - a data source `snowstate_outputs` as the `terraform_remote_state` substitute;
   - tests in `terraform-plugin-testing` state store mode, pinned to the latest alpha, run in a non-blocking CI job.
4. **Re-check at each Terraform minor release.** When PSS reaches a stable release, promote the provider and treat the HTTP server as the OpenTofu or compatibility path.
5. **Optional 1-day spike now:** build the provider with only an in-memory store, run it against `v1.17.0-alpha20260827`, and confirm the workflow (init, workspaces, lock conflict, `state migrate` from the HTTP backend). Do this if you want early confidence before committing to M7.

---

## Sources

- [HashiCorp: State stores overview (Plugin Framework)](https://developer.hashicorp.com/terraform/plugin/framework/state-stores)
- [HashiCorp: Implement state stores](https://developer.hashicorp.com/terraform/plugin/framework/state-stores/implementation)
- [HashiCorp: Acceptance tests, state store mode](https://developer.hashicorp.com/terraform/plugin/testing/acceptance-tests/state-store-mode)
- [HashiCorp Discuss: Experimental provider storing state in an OCI registry (Sep 2026)](https://discuss.hashicorp.com/t/experimental-provider-store-terraform-state-in-an-oci-registry-oras/77706)
- [vmvarela/terraform-provider-oras #36: tracking upstream PSS status](https://github.com/vmvarela/terraform-provider-oras/issues/36)
- [hashicorp/terraform-provider-tfcoremock PR #238: experimental `tfcoremock_fs` state store](https://github.com/hashicorp/terraform-provider-tfcoremock/pull/238)
- Source code read directly (shallow clones, 30–31 Oct 2026):
  - `hashicorp/terraform@main` (d40e1b2): `internal/command/arguments/init.go`, `internal/command/init_run.go`, `internal/configs/state_store.go`, `internal/backend/pluggable/`, `internal/states/remote/remote_grpc.go`, `internal/command/state_migrate.go`, `docs/plugin-protocol/tfplugin6.proto`, `CHANGELOG.md`, and the release tags
  - `hashicorp/terraform-plugin-framework@main`: the `statestore/` package and `CHANGELOG.md`
  - `snowflakedb/terraform-provider-snowflake@main`: `go.mod` and `main.go`
