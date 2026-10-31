package snowflake

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"

	"github.com/thunter/snowstate/pkg/snowstate/store"
)

// projectNameRE is a key segment, because a project is a key's first segment.
var projectNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Project is a row in PROJECTS.
type Project struct {
	Name, Schema, Role, PlanRole, Permissions string
}

// ProjectRequest asks for a new project.
type ProjectRequest struct {
	Name, Role, PlanRole string
	Permissions          []string
	CreatedBy            string
}

// Grants returns the statements for a role in a project schema (SPEC §5.3,
// §8.4.1). History stays append-only: no permission set can update or delete
// STATE_VERSIONS or AUDIT_LOG.
func (s *Store) Grants(schema, role string, perms []string) []string {
	has := map[string]bool{}
	for _, p := range perms {
		has[p] = true
	}
	db := s.opts.Database
	qualified := func(name string) string { return s.table(schema, name) }
	grant := func(priv, object string) string { return "GRANT " + priv + " ON " + object + " TO ROLE " + role }

	out := []string{
		grant("USAGE", "DATABASE "+db),
		grant("USAGE", "SCHEMA "+db+"."+schema),
		grant("USAGE", "SCHEMA "+db+"."+s.opts.ControlSchema),
		grant("SELECT", "TABLE "+s.controlTable("PROJECTS")),
		grant("SELECT", "TABLE "+s.controlTable("SCHEMA_MIGRATIONS")),
		grant("INSERT", "TABLE "+s.controlTable("AUDIT_LOG")),
		grant("INSERT", "TABLE "+qualified("AUDIT_LOG")),
	}
	if has["read"] {
		for _, t := range []string{"STATES", "STATE_VERSIONS", "LOCKS", "AUDIT_LOG"} {
			out = append(out, grant("SELECT", "TABLE "+qualified(t)))
		}
		out = append(out, grant("READ", "STAGE "+qualified("STATE_BLOBS")))
	}
	if has["lock"] {
		out = append(out, grant("SELECT, INSERT, DELETE", "TABLE "+qualified("LOCKS")))
	}
	if has["write"] {
		out = append(out,
			grant("SELECT, INSERT, UPDATE", "TABLE "+qualified("STATES")),
			grant("SELECT, INSERT", "TABLE "+qualified("STATE_VERSIONS")),
			grant("READ, WRITE", "STAGE "+qualified("STATE_BLOBS")),
		)
	}
	return out
}

// CreateProject creates a project schema, records it, grants the roles and
// regenerates the cross-project inventory view. An empty Role makes no grants,
// for an admin who manages them separately. The CLI always requires one.
func (s *Store) CreateProject(ctx context.Context, req ProjectRequest) (*Project, error) {
	if s.opts.Tenancy != TenancySchemaPerProject {
		return nil, fmt.Errorf("project create needs tenancy.mode=%s", TenancySchemaPerProject)
	}
	if !projectNameRE.MatchString(req.Name) || len(req.Name) > 128 {
		return nil, fmt.Errorf("%w: project %q", store.ErrInvalidKey, req.Name)
	}
	for _, role := range []string{req.Role, req.PlanRole} {
		if role == "" {
			continue
		}
		if err := validIdent(role); err != nil {
			return nil, err
		}
	}
	schema := ProjectSchema(req.Name)
	if err := s.checkCollision(ctx, req.Name, schema); err != nil {
		return nil, err
	}
	if err := s.MigrateSchema(ctx, schema); err != nil {
		return nil, err
	}
	if req.Role != "" {
		if err := s.applyGrants(ctx, s.Grants(schema, req.Role, req.Permissions)); err != nil {
			return nil, err
		}
	}
	if req.PlanRole != "" {
		if err := s.applyGrants(ctx, s.Grants(schema, req.PlanRole, []string{"read", "lock"})); err != nil {
			return nil, err
		}
	}
	p := &Project{
		Name: req.Name, Schema: schema, Role: req.Role, PlanRole: req.PlanRole,
		Permissions: strings.Join(req.Permissions, ","),
	}
	if _, err := s.exec(ctx, s.db, "project_create",
		"MERGE INTO "+s.controlTable("PROJECTS")+` t USING (SELECT ?::VARCHAR AS PROJECT, ?::VARCHAR AS SCHEMA_NAME,
			?::VARCHAR AS ROLE_NAME, ?::VARCHAR AS PLAN_ROLE, ?::VARCHAR AS PERMISSIONS, ?::VARCHAR AS CREATED_BY) s
		ON t.PROJECT = s.PROJECT
		WHEN MATCHED THEN UPDATE SET ROLE_NAME = s.ROLE_NAME, PLAN_ROLE = s.PLAN_ROLE, PERMISSIONS = s.PERMISSIONS
		WHEN NOT MATCHED THEN INSERT (PROJECT, SCHEMA_NAME, ROLE_NAME, PLAN_ROLE, PERMISSIONS, CREATED_BY)
		VALUES (s.PROJECT, s.SCHEMA_NAME, s.ROLE_NAME, s.PLAN_ROLE, s.PERMISSIONS, s.CREATED_BY)`,
		p.Name, p.Schema, p.Role, nullStr(p.PlanRole), p.Permissions, nullStr(req.CreatedBy)); err != nil {
		return nil, classify(err)
	}
	return p, s.regenerateInventory(ctx)
}

func (s *Store) applyGrants(ctx context.Context, stmts []string) error {
	for _, stmt := range stmts {
		if _, err := s.exec(ctx, s.db, "project_grant", stmt); err != nil {
			return classify(err)
		}
	}
	return nil
}

// checkCollision refuses a second project that sanitises to the same schema.
// "my-app" and "my_app" would otherwise share tables and defeat isolation.
func (s *Store) checkCollision(ctx context.Context, name, schema string) error {
	projects, err := s.ListProjects(ctx)
	if err != nil {
		return err
	}
	for _, p := range projects {
		if p.Schema == schema && p.Name != name {
			return fmt.Errorf("project %q maps to schema %s, already used by project %q", name, schema, p.Name)
		}
	}
	return nil
}

// ListProjects implements `project list`.
func (s *Store) ListProjects(ctx context.Context) ([]Project, error) {
	var out []Project
	err := retry(ctx, func() error {
		out = nil
		rows, err := s.query(ctx, s.db, "list_projects",
			"SELECT PROJECT, SCHEMA_NAME, ROLE_NAME, PLAN_ROLE, PERMISSIONS FROM "+s.controlTable("PROJECTS")+" ORDER BY PROJECT")
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var p Project
			var role, plan, perms sql.NullString
			if err := rows.Scan(&p.Name, &p.Schema, &role, &plan, &perms); err != nil {
				return err
			}
			p.Role, p.PlanRole, p.Permissions = role.String, plan.String, perms.String
			out = append(out, p)
		}
		return rows.Err()
	})
	return out, err
}

// regenerateInventory rebuilds the union view across project schemas.
func (s *Store) regenerateInventory(ctx context.Context) error {
	schemas, err := s.dataSchemas(ctx)
	if err != nil || len(schemas) == 0 {
		return err
	}
	parts := make([]string, 0, len(schemas))
	for _, schema := range schemas {
		parts = append(parts, "SELECT * FROM "+s.table(schema, "STATE_RESOURCES"))
	}
	_, err = s.exec(ctx, s.db, "project_view",
		"CREATE OR REPLACE SECURE VIEW "+s.controlTable("STATE_RESOURCES")+" AS "+strings.Join(parts, " UNION ALL "))
	return classify(err)
}
