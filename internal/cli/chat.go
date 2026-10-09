package cli

import (
	"context"
	"errors"
	"fmt"
	"os/exec"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ubixsys/ubixshepherd/internal/chat"
)

// shepherd chat: the one conversation, with the front desk and the swarm's events.
func runChat(ctx context.Context, env Env, args []string) error {
	fs := flags("chat", env)
	model := fs.String("model", "", "the front desk's model, over /model and desk.model in config.yaml")
	history := fs.Int("history", chat.DefaultHistory, "how many earlier entries to show on start")
	if pos, err := parse(fs, args); err != nil {
		return err
	} else if len(pos) > 0 {
		return errUsage
	}
	if !env.Interactive {
		return errors.New("shepherd chat needs a terminal")
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		return errors.New("shepherd chat's front desk runs on Claude Code, and claude is not on PATH")
	}
	if env.Exe == "" {
		return errors.New("cannot find this binary's path")
	}
	c, err := dial(ctx, env)
	if err != nil {
		return err
	}
	h, err := locate(ctx, env, c, "")
	if err != nil {
		return err
	}
	desk := chat.ClaudeDesk{Bin: bin, Shepherd: env.Exe, Dir: h.Workspace.Path}
	m := chat.New(ctx, c, desk, h.Workspace)
	m.ModelFlag = *model
	m.HistoryItems = *history
	chat.DetectBackground()
	restore := chat.SaveTitle(env.Stdout)
	defer restore()
	// Inline, not in the alternate screen: the conversation is printed into the terminal's
	// own scrollback, and the mouse stays the terminal's.
	if _, err := tea.NewProgram(m, tea.WithReportFocus(), tea.WithContext(ctx)).Run(); err != nil && !errors.Is(err, tea.ErrProgramKilled) {
		return fmt.Errorf("chat: %w", err)
	}
	return nil
}
