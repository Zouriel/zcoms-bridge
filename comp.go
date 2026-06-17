package main

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/Zouriel/zcoms-sdk/agent"
	"github.com/Zouriel/zcoms-sdk/ipc"
)

// comp is the bridge component's runtime. It owns the interactive per-user
// session state and reaches Telegram through the core daemon over IPC.
type comp struct {
	client        *ipc.Client
	waSocket      string
	waEnabled     bool
	bridgeBackend Backend
	triageBackend Backend
	locations     Locations
	allow         Allowlist
	agents        AgentConfig
	settings      Settings
	mainChatID    int64

	mu       sync.Mutex
	triageMu sync.Mutex
	byUser   map[int64]*userState
}

func (d *comp) send(chatID int64, text string) { _ = d.sendErr(chatID, text) }

func (d *comp) sendErr(chatID int64, text string) error {
	for _, part := range chunk(text, telegramMaxLen) {
		if _, err := d.client.Send(strconv.FormatInt(chatID, 10), part); err != nil {
			return err
		}
	}
	return nil
}

func (d *comp) sendFileTG(chatID int64, path, caption string) (string, error) {
	resp, err := d.client.SendFile(strconv.FormatInt(chatID, 10), path, caption)
	return resp.Label, err
}

func (d *comp) resolveChat(target string) (int64, int64, error) {
	id, err := d.client.Resolve(target)
	return id, id, err
}

func (d *comp) currentTriage() TriageSettings {
	if s, _, err := agent.LoadOrSeedSettings(); err == nil {
		return s.Triage
	}
	return d.settings.Triage
}

// errandCommand forwards an `errand …` command to the errands component over
// errands.sock and returns its reply (or a clear error string).
func (d *comp) errandCommand(text string) string {
	dir, err := agent.DefaultAppDir()
	if err != nil {
		return "⚠️ " + err.Error()
	}
	conn, err := net.DialTimeout("unix", filepath.Join(dir, "errands.sock"), 2*time.Second)
	if err != nil {
		return "The errands component isn't running — install it with `zc install errands`."
	}
	defer conn.Close()
	req, _ := json.Marshal(struct {
		Text string `json:"text"`
	}{text})
	_, _ = conn.Write(append(req, '\n'))
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	line, _ := bufio.NewReader(conn).ReadBytes('\n')
	var resp struct {
		OK    bool   `json:"ok"`
		Reply string `json:"reply"`
		Error string `json:"error"`
	}
	if json.Unmarshal(line, &resp) != nil {
		return "⚠️ couldn't reach the errands component"
	}
	if !resp.OK {
		return "⚠️ " + resp.Error
	}
	return resp.Reply
}

// handleErrandCommand relays an `errand …` bridge command to the errands component.
func (d *comp) handleErrandCommand(st *userState, text string) {
	d.send(st.chatID, d.errandCommand(text))
}

// triage-session.json helpers (the bridge resumes/resets the shared triage brain
// the same way the daemon and triage component do).
type triageSession struct {
	SessionID string    `json:"session_id"`
	UpdatedAt time.Time `json:"updated_at"`
}

func LoadTriageSessionID() (string, error) {
	dir, err := agent.DefaultAppDir()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(dir, "triage-session.json"))
	if err != nil {
		return "", nil
	}
	var s triageSession
	if json.Unmarshal(data, &s) != nil {
		return "", nil
	}
	return s.SessionID, nil
}

func SaveTriageSessionID(id string) error {
	if id == "" {
		return nil
	}
	dir, err := agent.DefaultAppDir()
	if err != nil {
		return err
	}
	return writeJSON(filepath.Join(dir, "triage-session.json"), triageSession{SessionID: id, UpdatedAt: time.Now()})
}

func ResetTriageSession() error {
	dir, err := agent.DefaultAppDir()
	if err != nil {
		return err
	}
	err = os.Remove(filepath.Join(dir, "triage-session.json"))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}
