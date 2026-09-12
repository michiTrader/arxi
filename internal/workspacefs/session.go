package workspacefs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/michiTrader/arxi/internal/workspace"
)

const markerName = ".arxi-workspace.json"

// Request is the stable, frozen identity of one member workspace.
type Request struct {
	JobID              string
	Member             string
	Mode               workspace.Mode
	ProfileID          string
	ProfileIdentity    string
	ProvisionerVersion string
	Command            *workspace.CommandProfile
	Source             workspace.SourceIdentity
}

// Session is an opaque verified workspace capability. Root is deliberately
// exposed only inside the repository; public host handles remain opaque.
type Session interface {
	WorkspaceRoot() (string, bool)
	Identity() string
	CommandProfile() (*workspace.CommandProfile, bool)
}

// Provisioner allocates and verifies stable member sessions.
type Provisioner interface {
	Provision(context.Context, Request) (Session, error)
	Release(context.Context, Request, Session) error
}

type session struct {
	id, root string
	hasRoot  bool
	command  *workspace.CommandProfile
}

func (s session) WorkspaceRoot() (string, bool) { return s.root, s.hasRoot }
func (s session) Identity() string              { return s.id }
func (s session) CommandProfile() (*workspace.CommandProfile, bool) {
	return s.command, s.command != nil
}

// OpenLocalSession is an internal adapter for already-selected verified roots.
func OpenLocalSession(root, identity string) (Session, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	canonicalRoot, err := canonical(root)
	if err != nil {
		return nil, err
	}
	return session{id: identity, root: canonicalRoot, hasRoot: true}, nil
}

// OpenLocalCommandSession is an internal adapter for an already-preflighted profile.
func OpenLocalCommandSession(root, identity string, command workspace.CommandProfile) (Session, error) {
	opened, err := OpenLocalSession(root, identity)
	if err != nil {
		return nil, err
	}
	local := opened.(session)
	local.command = &command
	return local, nil
}

type marker struct {
	Schema             string                    `json:"schema"`
	JobID              string                    `json:"job_id"`
	Member             string                    `json:"member"`
	Mode               workspace.Mode            `json:"mode"`
	ProfileID          string                    `json:"profile_id"`
	ProfileIdentity    string                    `json:"profile_identity"`
	ProvisionerVersion string                    `json:"provisioner_version"`
	Command            *workspace.CommandProfile `json:"command,omitempty"`
	Source             workspace.SourceIdentity  `json:"source"`
}

// Manager provisions source layouts beneath a private managed root.
type Manager struct {
	Root string
	mu   sync.Mutex
	live map[string]Session
}

func (m *Manager) Provision(ctx context.Context, req Request) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := validateRequest(req); err != nil {
		return nil, err
	}
	key := sessionKey(req)
	if existing := m.live[key]; existing != nil {
		return existing, nil
	}
	var got Session
	var err error
	switch req.Mode {
	case workspace.ModeNone:
		got = session{id: key, command: req.Command}
	case workspace.ModeShared:
		shared := req
		shared.Member = "shared"
		key = sessionKey(shared)
		if existing := m.live[key]; existing != nil {
			return existing, nil
		}
		got, err = m.provisionShared(ctx, shared, key)
	case workspace.ModeCopy:
		got, err = m.provisionCopy(ctx, req, key)
	case workspace.ModeWorktree:
		got, err = m.provisionWorktree(ctx, req, key)
	default:
		err = fmt.Errorf("workspace mode %q has no managed provisioner", req.Mode)
	}
	if err != nil {
		return nil, err
	}
	if m.live == nil {
		m.live = map[string]Session{}
	}
	m.live[key] = got
	return got, nil
}

func (m *Manager) provisionShared(ctx context.Context, req Request, key string) (Session, error) {
	shared := req
	shared.Mode = workspace.ModeCopy
	root := m.path(shared)
	if exists(root) {
		if err := verifyMarker(root, req); err != nil {
			return nil, err
		}
		if err := verifySnapshot(ctx, root, req); err != nil {
			return nil, err
		}
		return session{id: key, root: root, hasRoot: true, command: req.Command}, nil
	}
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		return nil, fmt.Errorf("create shared workspace parent: %w", err)
	}
	tmp := root + ".partial"
	if exists(tmp) {
		if err := os.RemoveAll(tmp); err != nil {
			return nil, fmt.Errorf("reconcile partial shared workspace: %w", err)
		}
	}
	if err := os.Mkdir(tmp, 0o700); err != nil {
		return nil, fmt.Errorf("create shared workspace: %w", err)
	}
	if err := copyTrackedTree(ctx, tmp, req.Source); err != nil {
		_ = os.RemoveAll(tmp)
		return nil, err
	}
	if err := writeMarker(tmp, req); err != nil {
		_ = os.RemoveAll(tmp)
		return nil, err
	}
	if err := os.Rename(tmp, root); err != nil {
		return nil, fmt.Errorf("publish shared workspace: %w", err)
	}
	return session{id: key, root: root, hasRoot: true, command: req.Command}, nil
}

func (m *Manager) provisionCopy(ctx context.Context, req Request, key string) (Session, error) {
	root := m.path(req)
	if exists(root) {
		if err := verifyMarker(root, req); err != nil {
			return nil, err
		}
		if err := verifySnapshot(ctx, root, req); err != nil {
			return nil, err
		}
		return session{id: key, root: root, hasRoot: true, command: req.Command}, nil
	}
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		return nil, fmt.Errorf("create workspace parent: %w", err)
	}
	tmp := root + ".partial"
	if exists(tmp) {
		if err := os.RemoveAll(tmp); err != nil {
			return nil, fmt.Errorf("reconcile partial copy workspace: %w", err)
		}
	}
	if err := os.Mkdir(tmp, 0o700); err != nil {
		return nil, fmt.Errorf("create copy workspace: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(tmp)
		}
	}()
	if err := copyTrackedTree(ctx, tmp, req.Source); err != nil {
		return nil, err
	}
	if err := writeMarker(tmp, req); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, root); err != nil {
		if exists(root) {
			if verifyErr := verifyMarker(root, req); verifyErr == nil {
				ok = true
				return session{id: key, root: root, hasRoot: true, command: req.Command}, nil
			}
		}
		return nil, fmt.Errorf("publish copy workspace: %w", err)
	}
	ok = true
	return session{id: key, root: root, hasRoot: true, command: req.Command}, nil
}

func (m *Manager) provisionWorktree(ctx context.Context, req Request, key string) (Session, error) {
	root := m.path(req)
	if exists(root) {
		if err := verifyMarker(root, req); err != nil {
			return nil, err
		}
		if err := verifyWorktree(ctx, root, req); err != nil {
			return nil, err
		}
		return session{id: key, root: root, hasRoot: true, command: req.Command}, nil
	}
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		return nil, fmt.Errorf("create worktree parent: %w", err)
	}
	cmd := exec.CommandContext(ctx, "git", "-C", req.Source.CanonicalRoot, "worktree", "add", "--detach", root, req.Source.Commit)
	body, commandErr := cmd.CombinedOutput()
	if commandErr != nil && !exists(root) {
		return nil, fmt.Errorf("create Git worktree: %w: %s", commandErr, strings.TrimSpace(string(body)))
	}
	if err := verifyWorktree(ctx, root, req); err != nil {
		return nil, fmt.Errorf("reconcile Git worktree creation: %w", err)
	}
	if err := writeMarker(root, req); err != nil {
		return nil, fmt.Errorf("record worktree ownership: %w", err)
	}
	return session{id: key, root: root, hasRoot: true, command: req.Command}, nil
}

func (m *Manager) Release(ctx context.Context, req Request, got Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := sessionKey(req)
	if got == nil || got.Identity() != key {
		return fmt.Errorf("workspace release handle does not belong to job %q member %q", req.JobID, req.Member)
	}
	if req.Mode == workspace.ModeNone || req.Mode == workspace.ModeShared {
		delete(m.live, key)
		return nil
	}
	root := m.path(req)
	if !exists(root) {
		delete(m.live, key)
		return nil
	}
	if err := verifyMarker(root, req); err != nil {
		return fmt.Errorf("refuse workspace release without exact ownership: %w", err)
	}
	if req.Mode == workspace.ModeWorktree {
		if err := verifyWorktree(ctx, root, req); err != nil {
			return err
		}
		cmd := exec.CommandContext(ctx, "git", "-C", req.Source.CanonicalRoot, "worktree", "remove", "--force", root)
		body, err := cmd.CombinedOutput()
		if err != nil && exists(root) {
			return fmt.Errorf("remove owned Git worktree: %w: %s", err, strings.TrimSpace(string(body)))
		}
	} else if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("remove owned copy workspace: %w", err)
	}
	delete(m.live, key)
	return nil
}

func validateRequest(req Request) error {
	if req.JobID == "" || req.Member == "" || req.ProfileID == "" || req.ProfileIdentity == "" || req.ProvisionerVersion == "" {
		return errors.New("workspace request requires job, member, profile, and provisioner identities")
	}
	if req.Mode != workspace.ModeNone && (req.Source.Kind != "git" || req.Source.CanonicalRoot == "" || req.Source.Commit == "" || req.Source.Tree == "") {
		return errors.New("workspace source layout requires a frozen Git root, commit, and tree")
	}
	return nil
}

func sessionKey(req Request) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{req.JobID, req.Member, string(req.Mode), req.ProfileID, req.ProfileIdentity, req.ProvisionerVersion, req.Source.CanonicalRoot, req.Source.Commit, req.Source.Tree}, "\x00")))
	return hex.EncodeToString(sum[:])
}

func (m *Manager) path(req Request) string {
	return filepath.Join(m.Root, safeComponent(req.JobID), safeComponent(req.Member), string(req.Mode))
}

func safeComponent(value string) string {
	clean := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '_'
	}, value)
	sum := sha256.Sum256([]byte(value))
	return clean + "-" + hex.EncodeToString(sum[:4])
}

func expectedMarker(req Request) marker {
	return marker{Schema: "arxi.workspace-owner/v1", JobID: req.JobID, Member: req.Member, Mode: req.Mode,
		ProfileID: req.ProfileID, ProfileIdentity: req.ProfileIdentity, ProvisionerVersion: req.ProvisionerVersion, Command: req.Command, Source: req.Source}
}

func writeMarker(root string, req Request) error {
	body, err := json.Marshal(expectedMarker(req))
	if err != nil {
		return err
	}
	path := filepath.Join(root, markerName)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create ownership marker: %w", err)
	}
	if _, err := file.Write(append(body, '\n')); err != nil {
		_ = file.Close()
		return fmt.Errorf("write ownership marker: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync ownership marker: %w", err)
	}
	return file.Close()
}

func verifyMarker(root string, req Request) error {
	body, err := os.ReadFile(filepath.Join(root, markerName))
	if err != nil {
		return fmt.Errorf("workspace path exists without a readable ownership marker: %w", err)
	}
	var got marker
	if err := json.Unmarshal(body, &got); err != nil {
		return fmt.Errorf("decode workspace ownership marker: %w", err)
	}
	wantBody, _ := json.Marshal(expectedMarker(req))
	gotBody, _ := json.Marshal(got)
	if string(gotBody) != string(wantBody) {
		return fmt.Errorf("workspace ownership marker does not match job %q member %q", req.JobID, req.Member)
	}
	return nil
}

func verifyWorktree(ctx context.Context, root string, req Request) error {
	observedRoot, err := gitOutput(ctx, root, "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("workspace is not a registered Git worktree: %w", err)
	}
	canonicalRoot, err := canonical(strings.TrimSpace(observedRoot))
	if err != nil || canonicalRoot != root {
		return fmt.Errorf("Git worktree root does not match managed workspace path")
	}
	common, err := gitOutput(ctx, root, "rev-parse", "--git-common-dir")
	if err != nil {
		return err
	}
	commonPath := strings.TrimSpace(common)
	if !filepath.IsAbs(commonPath) {
		commonPath = filepath.Join(root, commonPath)
	}
	commonPath, err = canonical(commonPath)
	if err != nil || commonPath != req.Source.CommonGitDir {
		return fmt.Errorf("Git worktree common directory does not match the frozen source")
	}
	head, err := gitOutput(ctx, root, "rev-parse", "HEAD^{commit}")
	if err != nil || strings.TrimSpace(head) != req.Source.Commit {
		return fmt.Errorf("Git worktree HEAD does not match frozen commit %s", req.Source.Commit)
	}
	return nil
}

func verifySnapshot(ctx context.Context, root string, req Request) error {
	entries, err := trackedEntries(ctx, req.Source)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(root, filepath.FromSlash(entry.path))
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("copy workspace is missing tracked path %q: %w", entry.path, err)
		}
		if entry.mode == "120000" && info.Mode()&os.ModeSymlink == 0 || entry.mode != "120000" && !info.Mode().IsRegular() {
			return fmt.Errorf("copy workspace tracked path %q has the wrong file type", entry.path)
		}
	}
	return nil
}

type trackedEntry struct {
	mode, object, path string
}

func trackedEntries(ctx context.Context, source workspace.SourceIdentity) ([]trackedEntry, error) {
	body, err := gitOutputBytes(ctx, source.CanonicalRoot, "ls-tree", "-rz", "--full-tree", source.Tree)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(string(body), "\x00")
	entries := make([]trackedEntry, 0, len(parts))
	folded := map[string]string{}
	for _, record := range parts {
		if record == "" {
			continue
		}
		tab := strings.IndexByte(record, '\t')
		if tab < 0 {
			return nil, fmt.Errorf("Git returned an invalid tracked-tree record")
		}
		fields := strings.Fields(record[:tab])
		if len(fields) != 3 {
			return nil, fmt.Errorf("Git returned an invalid tracked-tree record")
		}
		path := record[tab+1:]
		if err := safeRelative(path); err != nil {
			return nil, err
		}
		collisionKey := strings.ToLower(filepath.Clean(filepath.FromSlash(path)))
		if prior, found := folded[collisionKey]; found && prior != path {
			return nil, fmt.Errorf("tracked paths %q and %q collide on a case-insensitive filesystem", prior, path)
		}
		folded[collisionKey] = path
		if fields[1] != "blob" || fields[0] != "100644" && fields[0] != "100755" && fields[0] != "120000" {
			return nil, fmt.Errorf("tracked path %q has refused Git mode %s and type %s", path, fields[0], fields[1])
		}
		entries = append(entries, trackedEntry{mode: fields[0], object: fields[2], path: path})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	return entries, nil
}

func copyTrackedTree(ctx context.Context, destination string, source workspace.SourceIdentity) error {
	entries, err := trackedEntries(ctx, source)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(destination, filepath.FromSlash(entry.path))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fmt.Errorf("create tracked directory for %q: %w", entry.path, err)
		}
		body, err := gitOutputBytes(ctx, source.CanonicalRoot, "cat-file", "blob", entry.object)
		if err != nil {
			return fmt.Errorf("read tracked object for %q: %w", entry.path, err)
		}
		if entry.mode == "120000" {
			target := string(body)
			if err := safeSymlinkTarget(entry.path, target); err != nil {
				return err
			}
			if err := os.Symlink(filepath.FromSlash(target), path); err != nil {
				return fmt.Errorf("reproduce tracked symlink %q: %w", entry.path, err)
			}
			continue
		}
		mode := os.FileMode(0o600)
		if entry.mode == "100755" {
			mode = 0o700
		}
		if err := os.WriteFile(path, body, mode); err != nil {
			return fmt.Errorf("copy tracked file %q: %w", entry.path, err)
		}
	}
	return nil
}

func safeRelative(path string) error {
	if path == "" || filepath.IsAbs(filepath.FromSlash(path)) || strings.ContainsRune(path, 0) {
		return fmt.Errorf("tracked path %q is not a safe relative path", path)
	}
	clean := filepath.Clean(filepath.FromSlash(path))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("tracked path %q escapes the snapshot", path)
	}
	return nil
}

func safeSymlinkTarget(path, target string) error {
	if target == "" || filepath.IsAbs(filepath.FromSlash(target)) || strings.ContainsRune(target, 0) {
		return fmt.Errorf("tracked symlink %q has unsafe target %q", path, target)
	}
	resolved := filepath.Clean(filepath.Join(filepath.Dir(filepath.FromSlash(path)), filepath.FromSlash(target)))
	if resolved == ".." || strings.HasPrefix(resolved, ".."+string(filepath.Separator)) {
		return fmt.Errorf("tracked symlink %q escapes the snapshot through target %q", path, target)
	}
	return nil
}

func gitOutputBytes(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	body, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(body)))
	}
	return body, nil
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
