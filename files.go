package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
)

// uploadsSubdir is where files sent through the bridge are saved, inside the
// active project so the (sandboxed) agent can read them.
const uploadsSubdir = "zcoms-uploads"

// handleIncomingFile saves a file an allow-listed user sent (already downloaded
// by the daemon to localPath) into the active project and remembers it, so it's
// attached to the user's next turn (or run now if there's a caption).
func (d *comp) handleIncomingFile(st *userState, localPath, fileName, caption string) {
	d.mu.Lock()
	loc, dir, chatID := st.location, st.locationPath, st.chatID
	d.mu.Unlock()
	if loc == "" {
		d.send(chatID, "Pick a location first (send 'locations'), then send the file — it needs a project to live in.")
		return
	}
	if strings.TrimSpace(localPath) == "" {
		d.send(chatID, "⚠️ I couldn't get that file.")
		return
	}
	if fileName == "" {
		fileName = filepath.Base(localPath)
	}

	rel := filepath.Join(uploadsSubdir, sanitizeFilename(fileName))
	if err := copyInto(localPath, filepath.Join(dir, rel)); err != nil {
		d.send(chatID, "⚠️ Couldn't save the file: "+err.Error())
		return
	}

	d.mu.Lock()
	st.pendingFiles = append(st.pendingFiles, rel)
	d.mu.Unlock()

	if strings.TrimSpace(caption) != "" {
		d.send(chatID, "📎 Saved to "+rel+" — working on it…")
		d.dispatchAgentTurn(st, caption)
	} else {
		d.send(chatID, "📎 Saved to "+rel+" — tell me what to do with it.")
	}
}

// dispatchAgentTurn runs a message through the agent with confirm-aware routing
// (plan-first for the confirm role), attaching any files sent since the last turn.
func (d *comp) dispatchAgentTurn(st *userState, text string) {
	d.mu.Lock()
	role := st.effectiveRole
	files := st.pendingFiles
	st.pendingFiles = nil
	seed := st.triageSeed
	st.triageSeed = ""
	d.mu.Unlock()

	prompt := text
	if len(files) > 0 {
		prompt = "(Files I just sent you, saved in this project — read them from there: " +
			strings.Join(files, ", ") + ")\n\n" + text
	}
	if seed != "" {
		// First turn of an interactive-triage session: prepend the recipient
		// table + SEND-directive instructions ahead of the owner's instruction.
		prompt = seed + "\nThe owner says:\n" + prompt
	}

	role2 := role
	await := false
	if role == RoleConfirm {
		role2, await = RoleRead, true
	}

	if !d.runAgent(st, prompt, role2, await) && len(files) > 0 {
		// A run was already in flight — keep the files for the next turn so they
		// aren't lost (the message itself can be re-sent by the user).
		d.mu.Lock()
		st.pendingFiles = append(files, st.pendingFiles...)
		d.mu.Unlock()
	}
}

func sanitizeFilename(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	if name == "" || name == "." || name == ".." {
		return "file"
	}
	return strings.NewReplacer("/", "_", "\\", "_", "\n", " ", "\r", " ").Replace(name)
}

func copyInto(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}
