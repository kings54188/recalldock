package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	projectpkg "recalldock.local/recalldock/internal/project"
	"recalldock.local/recalldock/internal/filelock"
)

var ErrUnknownRegisteredProject = errors.New("unknown registered project")

type RegisteredProject struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
}

type ProjectBinding struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	RootPath  string `json:"root_path"`
}

type ProjectActivation struct {
	ReadBudgetID        string `json:"read_budget_id"`
	ReadBudgetLimit     int    `json:"read_budget_limit"`
	ReadBudgetExpiresAt string `json:"read_budget_expires_at"`
	ProjectID           string `json:"project_id"`
	ProjectName         string `json:"project_name"`
	Mode                string `json:"mode"`
	LoadsMemory         bool   `json:"loads_memory"`
}

func opaqueID(prefix string) (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(id[:]), nil
}

func (s *Store) CreateRegisteredProject(name string) (RegisteredProject, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 200 || strings.ContainsAny(name, "\x00\r\n") {
		return RegisteredProject{}, errors.New("project name must be a nonempty single line of at most 200 bytes")
	}
	id, err := opaqueID("prj-")
	if err != nil {
		return RegisteredProject{}, err
	}
	p := RegisteredProject{ID: id, Name: name, CreatedAt: Now()}
	err = s.withTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO project_identities(id,name,created_at) VALUES(?,?,?)`, p.ID, p.Name, p.CreatedAt); err != nil {
			return err
		}
		return refreshProjectAliasTx(tx, p.Name, p.ID)
	})
	return p, err
}

func (s *Store) RegisteredProject(id string) (RegisteredProject, error) {
	var p RegisteredProject
	err := s.db.QueryRow(`SELECT id,name,created_at FROM project_identities WHERE id=?`, id).Scan(&p.ID, &p.Name, &p.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrUnknownRegisteredProject
	}
	return p, err
}

func (s *Store) ListRegisteredProjects() ([]RegisteredProject, error) {
	rows, err := s.db.Query(`SELECT id,name,created_at FROM project_identities ORDER BY created_at,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []RegisteredProject{}
	for rows.Next() {
		var p RegisteredProject
		if err := rows.Scan(&p.ID, &p.Name, &p.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	return result, rows.Err()
}

// Activation is a read-only selection, not a persistent/global process switch
// and not a content load. Subsequent reads pass its explicit project ID.
func (s *Store) ActivateProjectReadOnly(id string, limits ...int) (ProjectActivation, error) {
	limit := DefaultReadBudget
	if len(limits) > 0 {
		limit = limits[0]
	}
	p, err := s.RegisteredProject(id)
	if err != nil {
		return ProjectActivation{}, err
	}
	budget, err := s.CreateReadBudget(p.ID, limit)
	return ProjectActivation{ReadBudgetID: budget.ID, ReadBudgetLimit: budget.Limit, ReadBudgetExpiresAt: budget.ExpiresAt, ProjectID: p.ID, ProjectName: p.Name, Mode: "read_only", LoadsMemory: false}, err
}

func (s *Store) DetectProject(dir string) projectpkg.DetectionResult {
	return projectpkg.DetectWithRegistry(dir, s)
}

// DetectExplicitProject selects an established binding or an explicit local
// declaration. Public memory reads must not use legacy name discovery.
func (s *Store) DetectExplicitProject(dir string) projectpkg.DetectionResult {
	return projectpkg.DetectExplicitWithRegistry(dir, s)
}

func (s *Store) ResolveProjectBinding(marker projectpkg.RootMarker, root string) (projectpkg.DetectionResult, error) {
	identity, err := s.Identity()
	if err != nil {
		return projectpkg.DetectionResult{}, err
	}
	return resolveProjectBindingRows(s.db, identity.StoreID, marker, root)
}

type projectBindingRows interface{ QueryRow(string, ...any) *sql.Row }

func resolveProjectBindingRows(rows projectBindingRows, storeID string, marker projectpkg.RootMarker, root string) (projectpkg.DetectionResult, error) {
	fail := func() (projectpkg.DetectionResult, error) {
		return projectpkg.DetectionResult{}, fmt.Errorf("%w: copied/moved/stale/foreign root; explicitly bind its project ID to this directory", projectpkg.ErrBinding)
	}
	if marker.StoreID != storeID {
		return fail()
	}
	var p RegisteredProject
	var path, device, inode, state string
	err := rows.QueryRow(`SELECT p.id,p.name,p.created_at,b.root_path,b.device,b.inode,b.state FROM project_bindings b JOIN project_identities p ON p.id=b.project_id WHERE b.id=? AND b.project_id=?`, marker.BindingID, marker.ProjectID).Scan(&p.ID, &p.Name, &p.CreatedAt, &path, &device, &inode, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return fail()
	}
	if err != nil {
		return projectpkg.DetectionResult{}, err
	}
	dev, ino, err := directoryIdentity(root)
	if err != nil {
		return projectpkg.DetectionResult{}, err
	}
	if state != "active" || !samePath(root, path) || dev != device || ino != inode {
		return fail()
	}
	return projectpkg.DetectionResult{Project: p.ID, ProjectID: p.ID, ProjectName: p.Name, BindingID: marker.BindingID, Source: projectpkg.SourceRegistered, Path: root}, nil
}

// BindProjectRoot is an explicit operator operation. SQLite owns identities;
// root.json only references one binding. A crash between file rename and SQL
// commit leaves an unverified marker that fails closed and can be rebound.
func (s *Store) BindProjectRoot(id, root string) (ProjectBinding, error) {
	if err := s.requireCurrentProjectKey(id); err != nil {
		return ProjectBinding{}, err
	}
	if _, err := s.RegisteredProject(id); err != nil {
		return ProjectBinding{}, err
	}
	if !filepath.IsAbs(root) {
		return ProjectBinding{}, errors.New("binding requires an absolute directory path")
	}
	if err := rejectUNCPath(root); err != nil {
		return ProjectBinding{}, err
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return ProjectBinding{}, err
	}
	root = normalizePath(root)
	home, _ := os.UserHomeDir()
	resolvedHome, _ := filepath.EvalSymlinks(home)
	if samePath(root, resolvedHome) || filepath.Dir(root) == root {
		return ProjectBinding{}, errors.New("binding HOME or filesystem root is not allowed")
	}
	// Shared root containers would let one project's root marker become a
	// foreign-root ancestor of every temporary/other project below it
	// (fail-closed for them, but useless noise). Binding them is never
	// meaningful: refuse up front.
	for _, shared := range []string{"/tmp", "/var/tmp", "/dev", "/proc", "/sys", "/run", "/mnt", "/media"} {
		if root == shared {
			return ProjectBinding{}, fmt.Errorf("binding shared root %q is not allowed; bind a project directory instead", root)
		}
	}
	device, inode, err := directoryIdentity(root)
	if err != nil {
		return ProjectBinding{}, err
	}
	hidden := filepath.Join(root, ".recalldock")
	if info, err := os.Lstat(hidden); err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return ProjectBinding{}, errors.New("unsafe binding metadata directory")
	}
	if err := os.MkdirAll(hidden, 0700); err != nil {
		return ProjectBinding{}, err
	}
	lockPath := filepath.Join(hidden, ".binding.lock")
	lock, err := filelock.Acquire(lockPath)
	if err != nil {
		if errors.Is(err, filelock.ErrUnavailable) {
			return ProjectBinding{}, fmt.Errorf("binding lock unavailable: another process is binding this root; retry after it exits")
		}
		return ProjectBinding{}, fmt.Errorf("binding lock: %w", err)
	}
	defer lock.Release()
	identity, err := s.Identity()
	if err != nil {
		return ProjectBinding{}, err
	}
	old, found, err := projectpkg.ReadRootMarker(root)
	if err != nil {
		return ProjectBinding{}, err
	}
	if found && old.StoreID != identity.StoreID {
		return ProjectBinding{}, errors.New("root already has another project/store marker; preserve it and explicitly resolve ownership first")
	}
	bindingID, err := opaqueID("bind-")
	if err != nil {
		return ProjectBinding{}, err
	}
	binding := ProjectBinding{ID: bindingID, ProjectID: id, RootPath: root}
	body, err := json.MarshalIndent(projectpkg.RootMarker{Version: 1, StoreID: identity.StoreID, ProjectID: id, BindingID: bindingID}, "", "  ")
	if err != nil {
		return ProjectBinding{}, err
	}
	err = s.withTx(func(tx *sql.Tx) error {
		if found && old.ProjectID != id {
			// Another project marker is replaceable only after its exact local
			// binding has been retired. Copied/foreign markers remain invalid.
			var valid int
			if err := tx.QueryRow(`SELECT count(*) FROM project_bindings WHERE id=? AND project_id=? AND root_path=? AND device=? AND inode=? AND state='superseded'`, old.BindingID, old.ProjectID, root, device, inode).Scan(&valid); err != nil {
				return err
			}
			if valid != 1 {
				return errors.New("root has another active or unverified project marker; explicitly unbind the selected binding first")
			}
			var active int
			if err := tx.QueryRow(`SELECT count(*) FROM project_bindings WHERE root_path=? AND state='active'`, root).Scan(&active); err != nil {
				return err
			}
			if active != 0 {
				return errors.New("root ownership changed; inspect current binding before retrying")
			}
		}
		// Supersede the previous active binding on this root path, and also a
		// same-project binding on the identical device+inode: a renamed or
		// moved directory keeps its inode, so the stale path row must not
		// stay active. Distinct inodes (genuine worktrees/copies) are untouched.
		if _, err := tx.Exec(`UPDATE project_bindings SET state='superseded'
			WHERE state='active' AND (root_path=? OR (project_id=? AND device=? AND inode=?))`,
			root, id, device, inode); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO project_bindings(id,project_id,root_path,device,inode,state) VALUES(?,?,?,?,?,'active')`, binding.ID, id, root, device, inode); err != nil {
			return err
		}
		return writeBindingMarker(hidden, append(body, '\n'))
	})
	return binding, err
}

func writeBindingMarker(hidden string, body []byte) error {
	tmp, err := os.CreateTemp(hidden, ".root-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(hidden, "root.json")); err != nil {
		return err
	}
	return syncDir(hidden)
}

// validateRegisteredWrite keeps identity writes on verified roots. Registered
// IDs must resolve to themselves; model transports additionally require an
// established binding (legacy name keys are no longer written by a model), so
// a first-time directory fails closed instead of splitting identity by name.
func (s *Store) validateRegisteredWrite(project, directory, origin string) error {
	if projectpkg.IsProjectID(project) {
		if _, err := s.RegisteredProject(project); err != nil {
			return err
		}
		resolved := s.DetectProject(directory)
		if resolved.Error != nil {
			return resolved.Error
		}
		if resolved.ProjectID != project {
			return fmt.Errorf("%w: registered project write requires a matching bound directory", projectpkg.ErrBinding)
		}
		return nil
	}
	if NormalizeSessionOrigin(origin) != SessionOriginModel {
		return nil
	}
	if directory == "" {
		return ErrProjectBindingRequired
	}
	resolved := s.DetectProject(directory)
	if resolved.Error != nil {
		return resolved.Error
	}
	switch resolved.Source {
	case projectpkg.SourceRegistered, projectpkg.SourceConfig, projectpkg.SourceWorktreeMain:
	default:
		return fmt.Errorf("%w: %q resolves by directory name only", ErrProjectBindingRequired, resolved.Project)
	}
	if resolved.Project != project {
		return fmt.Errorf("%w: session project does not match the bound directory", ErrProjectBindingRequired)
	}
	return nil
}

func importRegisteredProjects(tx *sql.Tx, identities []RegisteredProject) error {
	for _, p := range identities {
		if !projectpkg.IsProjectID(p.ID) || strings.TrimSpace(p.Name) == "" || len(p.Name) > 200 || strings.ContainsAny(p.Name, "\x00\r\n") {
			return errors.New("invalid exchanged project identity")
		}
		var name string
		err := tx.QueryRow(`SELECT name FROM project_identities WHERE id=?`, p.ID).Scan(&name)
		if err == nil {
			if name != p.Name {
				return fmt.Errorf("%w: project identity name differs", ErrImportIdentityConflict)
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if p.CreatedAt == "" {
			p.CreatedAt = Now()
		}
		if _, err := tx.Exec(`INSERT INTO project_identities(id,name,created_at) VALUES(?,?,?)`, p.ID, p.Name, p.CreatedAt); err != nil {
			return err
		}
		if err := refreshProjectAliasTx(tx, p.Name, p.ID); err != nil {
			return err
		}
	}
	return nil
}

func isRegisteredProjectID(id string) bool { return projectpkg.IsProjectID(id) }
