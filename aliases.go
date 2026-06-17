package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Zouriel/zcoms-sdk/agent"
)

// Aliases to the shared SDK types/funcs so the ported bridge code compiles
// against them with no edits.
type (
	Role           = agent.Role
	Backend        = agent.Backend
	AllowEntry     = agent.AllowEntry
	Allowlist      = agent.Allowlist
	Locations      = agent.Locations
	LocationConfig = agent.LocationConfig
	Session        = agent.Session
	RunResult      = agent.RunResult
	AgentConfig    = agent.AgentConfig
	Settings       = agent.Settings
	TriageSettings = agent.TriageSettings
)

const (
	RoleRead    = agent.RoleRead
	RoleConfirm = agent.RoleConfirm
	RoleEdit    = agent.RoleEdit
	RoleFull    = agent.RoleFull

	BackendClaude = agent.BackendClaude
	BackendCodex  = agent.BackendCodex
)

var (
	ListSessionsFor     = agent.ListSessionsFor
	RunAgent            = agent.RunAgent
	MinRole             = agent.MinRole
	ValidRole           = agent.ValidRole
	LoadOrSeedLocations = agent.LoadOrSeedLocations
	LoadOrSeedSettings  = agent.LoadOrSeedSettings
)

// roleRank mirrors the SDK's unexported Role.rank (read<confirm<edit<full), used
// to cap a user's role by a location ceiling.
func roleRank(r Role) int {
	switch r {
	case RoleRead:
		return 1
	case RoleConfirm:
		return 2
	case RoleEdit:
		return 3
	case RoleFull:
		return 4
	}
	return 0
}

func platformLabel(source string) string {
	if source == "wa" {
		return "WhatsApp"
	}
	return "Telegram"
}

func snippet(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// Recipient/TriageBatch mirror the triage component's last-triage.json so
// `interact triage` can reply to whoever wrote in.
type Recipient struct {
	Index    int      `json:"index"`
	Source   string   `json:"source"`
	Name     string   `json:"name"`
	TGChat   int64    `json:"tg_chat,omitempty"`
	WAChat   string   `json:"wa_chat,omitempty"`
	Messages []string `json:"messages"`
	Files    []string `json:"files,omitempty"`
}

type TriageBatch struct {
	At         time.Time   `json:"at"`
	Recipients []Recipient `json:"recipients"`
}

func configDir() (string, error) { return agent.DefaultAppDir() }

func LoadTriageBatch() (TriageBatch, error) {
	dir, err := configDir()
	if err != nil {
		return TriageBatch{}, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "last-triage.json"))
	if os.IsNotExist(err) {
		return TriageBatch{}, nil
	}
	if err != nil {
		return TriageBatch{}, err
	}
	var b TriageBatch
	if err := json.Unmarshal(data, &b); err != nil {
		return TriageBatch{}, err
	}
	return b, nil
}

func ensureStagingDir() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, "agent-staging")
	if err := os.MkdirAll(p, 0o700); err != nil {
		return "", err
	}
	return p, nil
}

// lockTriageBrain takes a blocking cross-process flock on the triage brain (the
// same lockfile the daemon and triage component use). Returns an unlock func, or
// nil on error (fail-open).
func lockTriageBrain() func() {
	dir, err := configDir()
	if err != nil {
		return nil
	}
	f, err := os.OpenFile(filepath.Join(dir, "triage-brain.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
}
