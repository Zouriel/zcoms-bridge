package main

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// isTeamCommand reports whether a message should be routed to the zc-team
// component (lowercased text).
func isTeamCommand(lower string) bool {
	switch lower {
	case "add task", "add tasks", "new task", "finish task", "team":
		return true
	}
	for _, p := range []string{"team ", "delegator ", "standup ", "staff ", "task ", "agent create "} {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}

// handleTeamCommand forwards a message to the team component over team.sock and
// relays the reply, staying in a "team session" while the component asks for more
// (multi-turn flows like add/new/finish task).
func (d *comp) handleTeamCommand(st *userState, text string) {
	dir, err := agent.DefaultAppDir()
	if err != nil {
		d.send(st.chatID, "⚠️ "+err.Error())
		return
	}
	conn, err := net.DialTimeout("unix", filepath.Join(dir, "team.sock"), 2*time.Second)
	if err != nil {
		d.setTeamSession(st, false)
		d.send(st.chatID, "The team component isn't running — install it with `zc install team`.")
		return
	}
	defer conn.Close()
	req, _ := json.Marshal(struct {
		Text  string `json:"text"`
		Actor string `json:"actor"`
	}{text, st.username})
	_, _ = conn.Write(append(req, '\n'))
	_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	line, _ := bufio.NewReader(conn).ReadBytes('\n')
	var resp struct {
		OK       bool   `json:"ok"`
		Reply    string `json:"reply"`
		Continue bool   `json:"continue"`
		Error    string `json:"error"`
	}
	if json.Unmarshal(line, &resp) != nil {
		d.setTeamSession(st, false)
		d.send(st.chatID, "⚠️ couldn't reach the team component")
		return
	}
	d.setTeamSession(st, resp.Continue)
	if !resp.OK {
		d.send(st.chatID, "⚠️ "+resp.Error)
		return
	}
	d.send(st.chatID, resp.Reply)
}

func (d *comp) setTeamSession(st *userState, on bool) {
	d.mu.Lock()
	st.teamSession = on
	d.mu.Unlock()
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
