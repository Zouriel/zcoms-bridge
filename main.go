// Command zcoms-bridge is the zcoms interactive bridge component: a standalone,
// pure-Go process that runs the agent-driving session state machine (locations,
// session resume, chat, interactive triage, file handling) for allow-listed
// users. It owns no Telegram session — the core daemon does and pushes the
// users' messages here over a subscribe stream; this replies via IPC.
package main

import (
	"log"
	"strings"
	"time"

	"github.com/Zouriel/zcoms-sdk/agent"
	"github.com/Zouriel/zcoms-sdk/ipc"
)

func main() {
	log.SetFlags(log.LstdFlags)
	log.Println("[bridge] component starting")

	client, err := ipc.NewDefault()
	if err != nil {
		log.Fatalf("[bridge] cannot resolve daemon socket: %v", err)
	}
	settings, _, err := agent.LoadOrSeedSettings()
	if err != nil {
		log.Fatalf("[bridge] settings: %v", err)
	}
	agents, _, _ := agent.LoadOrSeedAgents()
	locations, _, _ := agent.LoadOrSeedLocations()
	allow, _, _ := agent.LoadOrSeedAllowlist()

	d := &comp{
		client:        client,
		waSocket:      settings.WhatsApp.Socket,
		waEnabled:     settings.WhatsApp.Enabled,
		bridgeBackend: agents.For("bridge", ""),
		triageBackend: agents.For("triage", ""),
		locations:     locations,
		allow:         allow,
		agents:        agents,
		settings:      settings,
		byUser:        map[int64]*userState{},
	}
	if id, err := client.Resolve(settings.MainUser); err == nil {
		d.mainChatID = id
	}

	for {
		err := client.Subscribe("bridge", d.onEvent)
		log.Printf("[bridge] subscription ended (%v); reconnecting…", err)
		time.Sleep(5 * time.Second)
	}
}

// onEvent handles one allow-listed user's incoming message the daemon routed here.
func (d *comp) onEvent(ev ipc.Event) {
	st := d.stateFor(ev)
	if st == nil {
		return // not allow-listed (the daemon shouldn't route these, but guard)
	}
	if ev.Kind != "" && ev.Kind != "messageText" {
		// A file: the daemon already downloaded it to ev.File; ev.Text is the caption.
		d.handleIncomingFile(st, ev.File, "", ev.Text)
		return
	}
	d.handle(st, strings.TrimSpace(ev.Text))
}

// stateFor returns (creating on first contact) the per-user session state for an
// allow-listed sender.
func (d *comp) stateFor(ev ipc.Event) *userState {
	d.mu.Lock()
	defer d.mu.Unlock()
	if st, ok := d.byUser[ev.UserID]; ok {
		st.chatID = ev.ChatID
		return st
	}
	entry, ok := d.allow[ev.Sender]
	if !ok {
		return nil
	}
	st := &userState{
		username: ev.Sender,
		entry:    entry,
		chatID:   ev.ChatID,
		backend:  d.agents.For("bridge", entry.Agent),
	}
	d.byUser[ev.UserID] = st
	return st
}
