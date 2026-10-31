package sfconn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// GitHub's OIDC request variables, set when a job has `id-token: write`.
const (
	githubRequestURL   = "ACTIONS_ID_TOKEN_REQUEST_URL"
	githubRequestToken = "ACTIONS_ID_TOKEN_REQUEST_TOKEN"
)

const oidcFetchTimeout = 30 * time.Second

// oidcTokenFetcher returns the function that gets a token for each new
// connection, so a long apply never reuses an expired one.
func oidcTokenFetcher(wif WIF) func(context.Context) (string, error) {
	switch wif.TokenSource {
	case "github", "":
		return func(ctx context.Context) (string, error) { return githubToken(ctx, wif.Audience) }
	case "file":
		return func(context.Context) (string, error) {
			if wif.TokenFilePath == "" {
				return "", errors.New("snowflake: wif.token_source=file needs wif.token_file_path")
			}
			raw, err := os.ReadFile(wif.TokenFilePath)
			if err != nil {
				return "", fmt.Errorf("snowflake: reading OIDC token: %w", err)
			}
			return nonEmpty(string(raw))
		}
	case "env":
		return func(context.Context) (string, error) {
			if wif.TokenEnv == "" {
				return "", errors.New("snowflake: wif.token_source=env needs wif.token_env")
			}
			return nonEmpty(os.Getenv(wif.TokenEnv))
		}
	}
	return func(context.Context) (string, error) {
		return "", fmt.Errorf("snowflake: unknown wif.token_source %q", wif.TokenSource)
	}
}

func nonEmpty(token string) (string, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", errors.New("snowflake: the OIDC token is empty")
	}
	return token, nil
}

func githubToken(ctx context.Context, audience string) (string, error) {
	requestURL, bearer := os.Getenv(githubRequestURL), os.Getenv(githubRequestToken)
	if requestURL == "" || bearer == "" {
		return "", errors.New("snowflake: no GitHub OIDC environment: add `permissions: id-token: write` to the job")
	}
	if audience == "" {
		audience = DefaultWIFAudience
	}
	ctx, cancel := context.WithTimeout(ctx, oidcFetchTimeout)
	defer cancel()
	target := requestURL + "&audience=" + url.QueryEscape(audience)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil) //nolint:gosec // the URL comes from the runner
	if err != nil {
		return "", fmt.Errorf("snowflake: building OIDC request: %w", err)
	}
	req.Header.Set("Authorization", "bearer "+bearer)
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // the URL comes from the runner
	if err != nil {
		return "", fmt.Errorf("snowflake: fetching OIDC token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("snowflake: fetching OIDC token: HTTP %d", resp.StatusCode)
	}
	var body struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", errors.New("snowflake: fetching OIDC token: response is not JSON")
	}
	return nonEmpty(body.Value)
}
