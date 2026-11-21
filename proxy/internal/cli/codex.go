package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tokenmunchers/tokenmunchers/proxy/internal/codex"
	"github.com/tokenmunchers/tokenmunchers/proxy/internal/config"
)

// codexStatusTimeout bounds `codex login status`. It is a variable so tests
// can shorten it.
var codexStatusTimeout = 15 * time.Second

// codexOutcome says how Codex is set up after install.
type codexOutcome struct {
	// routed is the config.toml that routes Codex through the proxy; empty if
	// none does.
	routed string

	// unrouted is set when Codex is signed in with ChatGPT but was not routed
	// although install could have done it: the user said no, or writing failed.
	unrouted bool
}

// installCodex offers to route Codex, when it is signed in with ChatGPT,
// through the proxy by adding a Munch model provider to its user-level
// config.toml. Codex ignores the proxy environment variables in that mode.
// Anything that keeps it from doing so is reported and leaves Codex as it was;
// only a failed prompt is an error.
func (a *App) installCodex(p *prompter, paths config.Paths, cfg config.Config, noCodex bool) (codexOutcome, error) {
	var out codexOutcome
	if noCodex {
		a.stepf("Codex", "config not modified (--no-codex)")
		return out, nil
	}
	bin, ok := codex.Find()
	if !ok {
		return out, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), codexStatusTimeout)
	chatgpt, err := codex.SignedInWithChatGPT(ctx, bin)
	cancel()
	if err != nil {
		fmt.Fprintf(a.Stderr, "warning: could not tell whether Codex is signed in with ChatGPT, so its config was not changed: %v\n", err)
		return out, nil
	}
	if !chatgpt {
		a.stepf("Codex", "not signed in with ChatGPT (`codex login status`), so its config was not modified")
		return out, nil
	}
	if !codex.IsCanonicalUpstream(cfg.Routes["chatgpt"].Upstream) {
		a.stepf("Codex", "config not modified: the chatgpt route in %s uses a custom upstream, and munch will not configure Codex to send its sign-in there", paths.ConfigFile())
		return out, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	if home == "" && os.Getenv("CODEX_HOME") == "" {
		fmt.Fprintln(a.Stderr, "warning: cannot find your home directory, so Codex config was not changed")
		return out, nil
	}
	dir := codex.Home(os.Getenv, home)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		a.stepf("Codex", "%s is not a directory, so its config was not modified", dir)
		return out, nil
	}
	path := codex.ConfigPath(dir)

	key, ok := loadLocalKey(paths)
	if !ok {
		fmt.Fprintln(a.Stderr, "warning: cannot read munch's local key, so Codex config was not changed")
		return out, nil
	}
	plan, err := codex.Inspect(path, codex.BaseURL(cfg.ListenAddr, key.RoutePrefix()))
	if err != nil {
		fmt.Fprintf(a.Stderr, "warning: cannot read %s, so Codex was not set up: %v\n", path, err)
		return out, nil
	}
	switch {
	case plan.Status == codex.Blocked:
		a.stepf("Codex", "left %s as is: %s", path, plan.Reason)
		return out, nil
	case !plan.NeedsWrite() && plan.Manual:
		a.stepf("Codex", "already routed through munch by the provider you added to %s", path)
		out.routed = path
		return out, nil
	case !plan.NeedsWrite():
		a.stepf("Codex", "already routed through munch (%s)", path)
		out.routed = path
		return out, nil
	}

	ok, err = p.confirm(codexQuestion(path, plan))
	if err != nil {
		return out, err
	}
	if !ok {
		a.stepf("Codex", "config left as is")
		out.unrouted = true
		return out, nil
	}
	if err := codex.Apply(plan); err != nil {
		fmt.Fprintf(a.Stderr, "warning: could not update %s, so Codex was not set up: %v\n", path, err)
		out.unrouted = true
		return out, nil
	}
	switch {
	case plan.Has(codex.AddRoute):
		a.stepf("Codex", "added the Munch provider to %s", path)
	case plan.Has(codex.UpdateRoute):
		a.stepf("Codex", "pointed the Munch provider in %s at this proxy", path)
	}
	if plan.Has(codex.DropLegacyURL) {
		a.stepf("Codex", "removed the old plain-HTTP chatgpt_base_url from %s", path)
	}
	out.routed = path
	return out, nil
}

// codexQuestion describes the edit a plan makes to the Codex config at path.
func codexQuestion(path string, plan *codex.Plan) string {
	legacy := plan.Has(codex.DropLegacyURL)
	switch {
	case plan.Has(codex.AddRoute) && legacy:
		return fmt.Sprintf("Codex is signed in with ChatGPT. Add the Munch provider to %s and drop its old plain-HTTP chatgpt_base_url, so Codex goes through the proxy?", path)
	case plan.Has(codex.AddRoute):
		return fmt.Sprintf("Codex is signed in with ChatGPT. Add the Munch provider to %s so Codex goes through the proxy?", path)
	case plan.Has(codex.UpdateRoute) && legacy:
		return fmt.Sprintf("Point the Munch provider in %s at this proxy and drop its old plain-HTTP chatgpt_base_url?", path)
	case plan.Has(codex.UpdateRoute):
		return fmt.Sprintf("Point the Munch provider in %s at this proxy?", path)
	default:
		return fmt.Sprintf("Drop the old plain-HTTP chatgpt_base_url from %s? Codex rejects it.", path)
	}
}

// removeCodexRoute deletes the Codex settings install added and says which
// munch settings it had to leave in Codex's config: ones that were written by
// hand or edited since. Codex cannot reach a model through those any more.
func (a *App) removeCodexRoute() error {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	var dirs []string
	if os.Getenv("CODEX_HOME") != "" {
		dirs = append(dirs, codex.Home(os.Getenv, home))
	}
	if home != "" {
		if def := filepath.Join(home, ".codex"); len(dirs) == 0 || filepath.Clean(dirs[0]) != def {
			dirs = append(dirs, def)
		}
	}

	var errs []error
	for _, dir := range dirs {
		path := codex.ConfigPath(dir)
		res, err := codex.Remove(path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if res.Removed {
			fmt.Fprintf(a.Stdout, "Removed munch's settings from the Codex config %s; Codex processes that are already running keep using the proxy until you restart them.\n", path)
		}
		if len(res.Kept) > 0 {
			fmt.Fprintf(a.Stdout, "%s still has munch settings that were written by hand or edited (%s), so they were left alone. Codex fails to connect until you revert them; then start a new Codex process.\n", path, strings.Join(res.Kept, "; "))
		}
	}
	return errors.Join(errs...)
}
