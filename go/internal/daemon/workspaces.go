package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/robmorgan/agentd/go/internal/db"
	"github.com/robmorgan/agentd/go/internal/protocol"
	"github.com/robmorgan/agentd/go/internal/session"
)

// Workspaces are named directories on this machine, stored in state.db and
// managed by clients over the protocol. They exist so a client that does not
// share this machine's filesystem can still say where a session runs. They
// are addressing only: agentd does not create, clone or sync them.

func (s *Server) addWorkspace(req *protocol.AddWorkspace) (*session.Workspace, error) {
	name := strings.TrimSpace(req.Name)
	if !validSessionName(name) {
		return nil, fmt.Errorf("invalid workspace name `%s`: %s", name, sessionNameRules)
	}
	path, err := expandHome(req.Path)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("workspace path `%s` must be an absolute path or start with `~/`", req.Path)
	}
	path = filepath.Clean(path)
	if info, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("workspace path `%s` does not exist", path)
	} else if !info.IsDir() {
		return nil, fmt.Errorf("workspace path `%s` is not a directory", path)
	}
	w, err := s.db.AddWorkspace(name, path)
	if errors.Is(err, db.ErrWorkspaceExists) {
		return nil, fmt.Errorf("workspace `%s` already exists", name)
	}
	if err != nil {
		return nil, err
	}
	return &w, nil
}

func (s *Server) removeWorkspace(name string) error {
	removed, err := s.db.RemoveWorkspace(name)
	if err != nil {
		return err
	}
	if !removed {
		return fmt.Errorf("workspace `%s` not found", name)
	}
	return nil
}

// resolveCwd turns a create request's Cwd and Workspace into the absolute
// directory the agent runs in, and the workspace name to record. Everything
// is resolved on this machine: the client may be elsewhere, so its paths mean
// nothing here unless they are absolute or relative to a named workspace.
func (s *Server) resolveCwd(req *protocol.CreateSession) (string, *string, error) {
	var name string
	if req.Workspace != nil {
		name = strings.TrimSpace(*req.Workspace)
	}
	if name == "" {
		cwd, err := expandHome(req.Cwd)
		if err != nil {
			return "", nil, err
		}
		if !filepath.IsAbs(cwd) {
			return "", nil, fmt.Errorf("working directory `%s` must be an absolute path", req.Cwd)
		}
		return filepath.Clean(cwd), nil, nil
	}

	w, err := s.db.GetWorkspace(name)
	if err != nil {
		return "", nil, err
	}
	if w == nil {
		return "", nil, fmt.Errorf("workspace `%s` not found; add it with `agent workspace add`", name)
	}
	if req.Cwd == "" {
		return w.Path, &name, nil
	}
	if filepath.IsAbs(req.Cwd) || strings.HasPrefix(req.Cwd, "~") {
		return "", nil, fmt.Errorf("working directory `%s` must be relative to workspace `%s`", req.Cwd, name)
	}
	// A lexical check, to catch `../` mistakes; symlinks inside the
	// workspace are followed like anywhere else.
	cwd := filepath.Join(w.Path, req.Cwd)
	if rel, err := filepath.Rel(w.Path, cwd); err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", nil, fmt.Errorf("working directory `%s` is outside workspace `%s`", req.Cwd, name)
	}
	return cwd, &name, nil
}

// expandHome replaces a leading `~` or `~/` with the daemon user's home
// directory. Paths are resolved on the daemon's machine, never the client's.
// `~user` is not supported and is returned unchanged.
func expandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot expand `%s`: %w", path, err)
	}
	return filepath.Join(home, path[1:]), nil
}
