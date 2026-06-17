package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const telegramMaxLen = 4000

// userState tracks one allow-listed user's place in the bridge.
type userState struct {
	username string
	entry    AllowEntry
	chatID   int64

	location      string // active location name ("" = none picked)
	locationPath  string
	effectiveRole Role    // user role capped by the location's max_role
	backend       Backend // resolved agent backend ("" = none installed)
	forceBackend  Backend // per-session backend override (e.g. `chat` pins codex); "" = use backend
	sessionID     string  // active agent session ("" = will start fresh)

	pendingKind     string    // "location" | "session" | ""
	pendingLoc      []string  // location names awaiting numeric pick
	pendingSess     []Session // sessions awaiting numeric pick
	pendingFiles    []string  // files saved to zcoms-uploads/, to attach to the next turn
	busy            bool      // an agent run is in flight
	awaitingConfirm bool      // a plan is waiting for the user's yes/no (confirm role)

	// Interactive-triage session: when set, agent turns may emit `SEND <idx> | text`
	// directives that the daemon routes to the batch's recipients (and only them).
	triageReply      bool
	triageRecipients []Recipient
	triageSeed       string // system prompt prepended to the first turn, then cleared
	triageSession    bool   // this session IS the persistent triage brain (interact triage)

	teamSession bool // mid multi-turn zc-team conversation: route messages to team.sock
}

// RunDaemon resolves the allow-list, greets each member, then services incoming
// messages until interrupted.
// dispatchUpdate handles one incoming TDLib update. It runs under a recover so a
// panic parsing untrusted message JSON can never crash the daemon's receive loop.
func (d *comp) handle(st *userState, text string) {
	if text == "" {
		return
	}
	lower := strings.ToLower(text)

	// Errand management commands work any time (they don't touch the user's own
	// agent session), so handle them before the busy gate.
	if lower == "errand" || lower == "errands" || strings.HasPrefix(lower, "errand ") {
		d.handleErrandCommand(st, strings.TrimSpace(text))
		return
	}

	// zc-team commands (and any ongoing multi-turn team conversation) are
	// forwarded to the team component, which holds the conversation state.
	if st.teamSession || isTeamCommand(lower) {
		d.handleTeamCommand(st, strings.TrimSpace(text))
		return
	}

	d.mu.Lock()
	busy := st.busy
	d.mu.Unlock()
	if busy {
		// Allow only status while a run is in flight.
		if lower == "status" {
			d.send(st.chatID, d.statusLine(st))
			return
		}
		d.send(st.chatID, "⏳ Still working on your previous message — one moment.")
		return
	}

	switch lower {
	case "help", "?", "/help", "start", "/start":
		d.send(st.chatID, d.helpText(st))
		return
	case "locations", "loc", "/locations":
		d.listLocations(st)
		return
	case "resume", "sessions", "/resume":
		d.listSessions(st)
		return
	case "new", "/new":
		d.startNew(st)
		return
	case "end", "stop", "exit", "/end":
		d.mu.Lock()
		st.sessionID, st.pendingKind, st.awaitingConfirm = "", "", false
		st.triageReply, st.triageRecipients, st.triageSeed, st.triageSession = false, nil, "", false
		st.forceBackend = ""
		d.mu.Unlock()
		d.send(st.chatID, "Detached. Send 'locations' to pick where to work.")
		return
	case "status", "/status":
		d.send(st.chatID, d.statusLine(st))
		return
	case "interact triage", "interact-triage", "triage-reply":
		d.startTriageReply(st)
		return
	case "chat", "/chat":
		d.startChat(st)
		return
	case "triage reset", "reset triage", "triage-reset", "new triage":
		d.resetTriageBrain(st)
		return
	}

	// Numeric selection from a pending menu.
	if st.pendingKind != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(text)); err == nil {
			d.selectNumber(st, n)
			return
		}
	}

	// Otherwise it's a message for the agent.
	d.mu.Lock()
	loc := st.location
	awaiting := st.awaitingConfirm
	d.mu.Unlock()
	if loc == "" {
		d.send(st.chatID, "Pick a location first — send 'locations'.")
		return
	}

	// Confirm role: a plan is awaiting your yes/no.
	if awaiting {
		switch lower {
		case "yes", "y", "go", "ok", "okay", "proceed", "do it":
			d.mu.Lock()
			st.awaitingConfirm = false
			// Execute with the user's real power so the run doesn't stall on
			// actions acceptEdits won't auto-approve (at least edit-level).
			execRole := st.entry.Role
			if roleRank(execRole) < roleRank(RoleEdit) {
				execRole = RoleEdit
			}
			d.mu.Unlock()
			d.runAgent(st, "Go ahead and carry out that plan now.", execRole, false)
		case "no", "n", "cancel", "nope", "abort":
			d.mu.Lock()
			st.awaitingConfirm = false
			d.mu.Unlock()
			d.send(st.chatID, "Cancelled. Send a new message when ready.")
		default:
			d.dispatchAgentTurn(st, text) // refine -> re-plan (attaches any files)
		}
		return
	}

	d.dispatchAgentTurn(st, text)
}

func (d *comp) listLocations(st *userState) {
	// Reload so `zc locations add/remove` applies without a daemon restart.
	if locs, _, err := LoadOrSeedLocations(); err == nil {
		d.locations = locs
	}
	names := d.locations.SortedNames()
	var allowed []string
	for _, name := range names {
		if st.entry.AllowsLocation(name) {
			allowed = append(allowed, name)
		}
	}
	if len(allowed) == 0 {
		d.send(st.chatID, "No locations are configured for you. (Edit agent-locations.json / your allowlist entry.)")
		return
	}

	var b strings.Builder
	b.WriteString("📂 Locations — reply with a number:\n")
	for i, name := range allowed {
		cfg := d.locations[name]
		cap := ""
		if ValidRole(cfg.MaxRole) {
			cap = "  [max: " + string(cfg.MaxRole) + "]"
		}
		fmt.Fprintf(&b, "  %d. %s  (%s)%s\n", i+1, name, cfg.Path, cap)
	}
	d.mu.Lock()
	st.pendingKind, st.pendingLoc, st.pendingSess = "location", allowed, nil
	d.mu.Unlock()
	d.send(st.chatID, b.String())
}

func (d *comp) listSessions(st *userState) {
	d.mu.Lock()
	loc, path := st.location, st.locationPath
	d.mu.Unlock()
	if loc == "" {
		d.send(st.chatID, "Pick a location first — send 'locations'.")
		return
	}

	sessions, err := ListSessionsFor(st.entry.Agent, path, 12)
	if err != nil {
		d.send(st.chatID, "⚠️ Couldn't list sessions: "+err.Error())
		return
	}
	if len(sessions) == 0 {
		d.send(st.chatID, "No past sessions in "+loc+". Just send a message to start a new one.")
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "🗂 Sessions in %s — reply with a number:\n", loc)
	for i, s := range sessions {
		fmt.Fprintf(&b, "  %d. %s  (%s)\n", i+1, s.Title, humanAgo(s.Modified))
	}
	d.mu.Lock()
	st.pendingKind, st.pendingSess, st.pendingLoc = "session", sessions, nil
	d.mu.Unlock()
	d.send(st.chatID, b.String())
}

func (d *comp) selectNumber(st *userState, n int) {
	// Snapshot the pending menu under the lock so the range-check and indexing
	// below can't race a concurrent listLocations/listSessions reassigning them.
	d.mu.Lock()
	kind := st.pendingKind
	pendingLoc := st.pendingLoc
	pendingSess := st.pendingSess
	d.mu.Unlock()

	switch kind {
	case "location":
		if n < 1 || n > len(pendingLoc) {
			d.send(st.chatID, "Out of range. Send 'locations' again.")
			return
		}
		name := pendingLoc[n-1]
		cfg := d.locations[name]
		role := st.entry.Role
		if ValidRole(cfg.MaxRole) {
			role = MinRole(role, cfg.MaxRole)
		}
		d.mu.Lock()
		st.location, st.locationPath, st.effectiveRole = name, cfg.Path, role
		st.sessionID, st.pendingKind, st.awaitingConfirm = "", "", false
		st.forceBackend = "" // a picked location uses the user's default backend
		d.mu.Unlock()
		d.send(st.chatID, fmt.Sprintf("📍 %s (%s)\nRole here: %s\nSend 'resume' to continue a past session, or just send a message to start a new one.", name, cfg.Path, role))

	case "session":
		if n < 1 || n > len(pendingSess) {
			d.send(st.chatID, "Out of range. Send 'resume' again.")
			return
		}
		sess := pendingSess[n-1]
		d.mu.Lock()
		st.sessionID, st.pendingKind = sess.ID, ""
		d.mu.Unlock()
		d.send(st.chatID, fmt.Sprintf("↩️ Resuming: %s\nFetching a summary…", sess.Title))
		d.runAgent(st, "Briefly summarize in 2-4 sentences what we were last working on in this conversation and what the current state / next step is. Don't take any actions.", RoleRead, false)

	default:
		d.send(st.chatID, "Nothing to select. Send 'help'.")
	}
}

// startChat puts the user in a full-power, general-purpose agent session: their
// allow-listed role (full for the owner) in their home directory, on the bridge
// session-type agent (`zc agents set bridge`). Unlike `interact triage`, it is a normal agent
// session with no directive protocol — it can create/edit files, run commands,
// SSH into servers, etc. A context seed (first turn) tells it the owner's
// Telegram/WhatsApp are already wired through this tool. Resumes until `new`/`end`.
func (d *comp) startChat(st *userState) {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		home = d.currentTriage().Dir // sensible fallback
	}
	d.mu.Lock()
	st.location, st.locationPath, st.effectiveRole = "chat", home, st.entry.Role
	st.sessionID, st.pendingKind, st.awaitingConfirm = "", "", false
	st.triageReply, st.triageSession, st.triageRecipients = false, false, nil
	st.forceBackend = d.bridgeBackend // chat is a bridge session — use the bridge agent
	st.triageSeed = buildChatSeed()
	role := st.effectiveRole
	d.mu.Unlock()
	d.send(st.chatID, fmt.Sprintf(
		"💬 Chat on — general-purpose assistant in %s (role: %s).\n"+
			"I can create/edit files, run commands, SSH into servers, etc. "+
			"Send `new` to reset the session or `end` to detach.", home, role))
}

// buildChatSeed is prepended to the chat session's first turn so the agent knows
// the owner's Telegram and WhatsApp are ALREADY connected through this tool and
// can be reached with the `zc` CLI — instead of telling the owner to log in.
func buildChatSeed() string {
	return strings.Join([]string{
		"You are the owner's personal assistant, running on their own machine via the `zc`",
		"bridge with full shell access. Their Telegram AND WhatsApp are ALREADY logged in",
		"through this tool — never tell them to log in, open WhatsApp Web, or scan a QR.",
		"To reach their messages, use the `zc` CLI (it routes through the running daemon and",
		"the paired WhatsApp sidecar, so no login is needed):",
		"  • WhatsApp: `zc wa unread` (list unread) · `zc wa send <number|jid> <msg>` · `zc wa send-file <number|jid> <path>`",
		"  • Telegram: `zc tg chat <@user|id> --read N` (history) · `zc tg send <@user|id> <msg>` · `zc tg send-file <@user|id> <path>`",
		"",
		"ERRANDS — when the owner asks you to message someone, ask them a set of things, and/or",
		"produce something from their answers (e.g. \"ask my wife what's needed for her CV, make it,",
		"send it to her, and ping me when done\"), dispatch an errand instead of doing it inline:",
		"  `zc errand start [deliver] [go] <@user|wa:NUMBER|#index> | <brief>`",
		"    deliver = also send the finished file to the contact · go = skip the approval step and start now.",
		"An errand runs in two sandboxed agents: an INTERVIEWER (no filesystem/shell — it only chats,",
		"greeting the contact and asking ONE question at a time with a remaining count, recording answers",
		"to a single file), then a PRODUCER that treats those answers as untrusted third-party DATA, does",
		"only the brief you gave, flags anything suspicious or mismatched, builds the deliverable, and",
		"sends you the file(s) + a summary when done. Because the contact isn't the owner, write the brief",
		"precisely — it's the only instruction the producer is allowed to act on. Manage with",
		"`zc errand list` / `zc errand cancel <id>`. Prefer this for any \"go talk to X and come back with Y\"",
		"task — don't try to hold the back-and-forth yourself.",
		"For anything else, you have a normal shell — create/edit files, run commands, SSH, etc.",
	}, "\n")
}

func (d *comp) startNew(st *userState) {
	d.mu.Lock()
	loc := st.location
	st.sessionID, st.pendingKind, st.awaitingConfirm = "", "", false
	d.mu.Unlock()
	if loc == "" {
		d.send(st.chatID, "Pick a location first — send 'locations'.")
		return
	}
	d.send(st.chatID, "🆕 New session in "+loc+". Send your first message.")
}

// runAgent runs one agent turn in the background and posts the reply. When
// awaitConfirmAfter is set the run is a plan (role read) and, on success, the
// user is asked to approve before anything executes.
// runAgent starts one agent turn in the background and returns whether it
// started (false only when a run is already in flight).
func (d *comp) runAgent(st *userState, prompt string, role Role, awaitConfirmAfter bool) bool {
	d.mu.Lock()
	if st.busy {
		d.mu.Unlock()
		return false // a run is already in flight; don't start a second
	}
	st.busy = true
	backend := st.backend
	dir, resume, chatID := st.locationPath, st.sessionID, st.chatID
	triageReply := st.triageReply
	triageSession := st.triageSession
	recipients := st.triageRecipients
	// Backend precedence: an explicit per-session override (e.g. `chat` pins
	// codex) wins; otherwise interactive-triage runs on the triage backend so it
	// can resume the codex triage brain; otherwise the user's default backend.
	if st.forceBackend != "" {
		backend = st.forceBackend
	} else if triageReply && d.triageBackend != "" {
		backend = d.triageBackend
	}
	d.mu.Unlock()
	if backend == "" {
		d.mu.Lock()
		st.busy = false
		d.mu.Unlock()
		d.send(chatID, "⚠️ Agent mode is unavailable — no `claude` or `codex` CLI is installed.")
		return true // turn consumed (nothing to retry)
	}

	if awaitConfirmAfter {
		d.send(chatID, "🧭 planning…")
	} else {
		d.send(chatID, "🤔 working…")
	}

	go func() {
		// A panic while parsing agent/TDLib output must not crash the whole daemon
		// (all users) or leave this user wedged at busy=true forever. Registered
		// first so it runs last on unwind — after any triageMu unlock below.
		defer func() {
			if r := recover(); r != nil {
				fmt.Printf("[bridge] recovered from panic in agent turn: %v\n", r)
				d.mu.Lock()
				st.busy = false
				d.mu.Unlock()
				d.send(chatID, "⚠️ Something went wrong handling that turn — please try again.")
			}
		}()

		// The triage brain is a single shared session — serialize so a scheduled
		// pass and this interactive turn never drive it at once. triageMu guards
		// in-process; the flock also guards the external triage component.
		if triageSession {
			d.triageMu.Lock()
			defer d.triageMu.Unlock()
			if unlock := lockTriageBrain(); unlock != nil {
				defer unlock()
			}
		}
		// recordSession persists the (possibly new) session id after each turn so
		// resumes — and the read-loop below — keep continuing the same conversation.
		curSession := resume
		recordSession := func(r RunResult) {
			if r.SessionID == "" {
				return
			}
			curSession = r.SessionID
			d.mu.Lock()
			st.sessionID = r.SessionID
			d.mu.Unlock()
			if triageSession {
				_ = SaveTriageSessionID(r.SessionID)
			}
		}

		// Interactive triage/chat agents get a writable staging dir as their cwd so
		// they can produce files (e.g. screenshots) to SENDFILE — still no network.
		runDir, stagingWritable := dir, false
		if triageReply {
			if sd, serr := ensureStagingDir(); serr == nil {
				runDir, stagingWritable = sd, true
			}
		}

		res, err := RunAgent(backend, runDir, prompt, resume, role, stagingWritable)
		recordSession(res)

		// Triage/chat agents are sandboxed and can't reach Telegram. When the agent
		// emits READ directives, the daemon fetches that history and feeds it back,
		// resuming the same session, until the agent stops asking (or the cap hits).
		if err == nil && triageReply {
			for round := 0; round < maxTriageReadRounds; round++ {
				reads := parseReadDirectives(res.Text)
				if len(reads) == 0 {
					break
				}
				d.send(chatID, "🔎 reading…")
				feedback := d.runTriageReads(reads)
				res, err = RunAgent(backend, runDir, feedback, curSession, role, stagingWritable)
				recordSession(res)
				if err != nil {
					break
				}
			}
			// If the agent still wants to read after the cap, it would otherwise
			// answer referencing history it never received. Tell it to wrap up.
			if err == nil && len(parseReadDirectives(res.Text)) > 0 {
				res, err = RunAgent(backend, runDir,
					"Read limit reached for this turn — no more chats can be fetched now. "+
						"Answer the owner using only what you already have (or SEND if asked).",
					curSession, role, stagingWritable)
				recordSession(res)
			}
		}

		d.mu.Lock()
		st.busy = false
		if err == nil && awaitConfirmAfter {
			st.awaitingConfirm = true
		}
		d.mu.Unlock()

		if err != nil {
			if res.Text != "" {
				d.send(chatID, res.Text)
			}
			d.send(chatID, "⚠️ "+err.Error())
			return
		}
		if strings.TrimSpace(res.Text) == "" {
			d.send(chatID, "(no output)")
			return
		}
		if triageReply {
			// Intercept SEND directives, route them to the batch's recipients,
			// strip them from view, and post the result + send confirmations.
			d.handleTriageReplyOutput(chatID, recipients, res.Text)
			return
		}
		d.send(chatID, res.Text)
		if awaitConfirmAfter {
			d.send(chatID, "✅ Reply 'yes' to carry this out, 'no' to cancel, or send changes to refine the plan.")
		}
	}()
	return true
}

func (d *comp) statusLine(st *userState) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	loc := st.location
	if loc == "" {
		loc = "(none)"
	}
	sess := st.sessionID
	if sess == "" {
		sess = "(new on next message)"
	}
	role := st.entry.Role
	if st.location != "" {
		role = st.effectiveRole
	}
	return fmt.Sprintf("Role: %s\nLocation: %s\nSession: %s", role, loc, sess)
}

func (d *comp) helpText(st *userState) string {
	return strings.Join([]string{
		"🤖 Agent bridge — commands:",
		"  locations — pick a project to work in",
		"  resume — continue a past session (with a summary)",
		"  new — start a fresh session here",
		"  status — show current location/session",
		"  end — detach from the current session",
		"  interact triage — talk to the triage agent (same memory) & reply to people",
		"  chat — full general-purpose assistant (files, shell, SSH) in your home dir",
		"  triage reset — wipe the triage agent's memory (fresh session next pass)",
		"Anything else you type is sent to the agent.",
		"",
		"Your role: " + string(st.entry.Role),
	}, "\n")
}

// send posts text to a chat, splitting anything over Telegram's length limit.
func chunk(s string, max int) []string {
	if len(s) <= max {
		return []string{s}
	}
	var out []string
	for len(s) > max {
		cut := strings.LastIndex(s[:max], "\n")
		if cut <= 0 {
			cut = max
		}
		out = append(out, s[:cut])
		s = s[cut:]
	}
	if strings.TrimSpace(s) != "" {
		out = append(out, s)
	}
	return out
}

func humanAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
