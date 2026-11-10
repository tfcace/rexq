package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/tfcace/rexq/internal/agent"
	"github.com/tfcace/rexq/internal/extract"
)

const configUsage = `usage:
  rexq config get pi-model [--json --provider P --session-model M]
  rexq config set pi-model provider/model|auto
  rexq config ask pi-model [--provider P --session-model M]
  rexq config dismiss pi-model

pi-model is the model Pi extractions run with. Unset ("auto"), they use a
cheap model of the session's provider, else the session's model.
REXQ_PI_MODEL in the environment overrides it. --json also prints what
"auto" resolves to for a session of provider P and model M, the override,
the model an extraction would use, and whether the user was asked
(never, open, done).

Pi's extension asks once per store: ask prints get's JSON with "ask": true
for the one caller that should show the picker, and marks it asked; set
(auto included) or dismiss answers it.`

// configMain reads and writes rexq's settings in the store.
func configMain(args []string) error {
	if len(args) < 2 || args[1] != agent.Pi.ModelSetting {
		return errors.New(configUsage)
	}
	switch args[0] {
	case "get":
		return configGet(args[2:], false)
	case "ask":
		return configGet(args[2:], true)
	case "dismiss":
		if len(args) != 2 {
			return errors.New(configUsage)
		}
		s := openStore()
		defer s.Close()
		return s.DismissAsk(agent.Pi.ModelSetting)
	case "set":
		if len(args) != 3 {
			return errors.New(configUsage)
		}
		return configSet(args[2])
	}
	return errors.New(configUsage)
}

// configGet prints the setting, or with ask, claims the first-run picker
// and prints the JSON.
func configGet(args []string, ask bool) error {
	fs := flag.NewFlagSet("config get", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the setting, the automatic model, the override and the model in effect")
	provider := fs.String("provider", "", "the session's provider, for --json")
	session := fs.String("session-model", "", "the session's model, for --json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return errors.New(configUsage)
	}
	s := openStore()
	defer s.Close()
	key := agent.Pi.ModelSetting
	claimed := false
	if ask {
		var err error
		if claimed, err = s.ClaimAsk(key, time.Now()); err != nil {
			return err
		}
	}
	v, err := s.Setting(key)
	if err != nil {
		return err
	}
	asked, err := s.AskState(key, time.Now())
	if err != nil {
		return err
	}
	if !*asJSON && !ask {
		fmt.Println(v)
		return nil
	}
	c := extract.ModelChoice{Env: os.Getenv(extract.PiModelEnv), Configured: v, Provider: *provider, Session: *session}
	effective := ""
	if m := extract.PiModels(c); len(m) > 0 {
		effective = m[0]
	}
	out := map[string]any{
		key:         v,
		"automatic": extract.PiAutomatic(*provider, *session),
		"override":  strings.TrimSpace(c.Env),
		"effective": effective,
		"asked":     asked,
	}
	if ask {
		out["ask"] = claimed
	}
	return json.NewEncoder(os.Stdout).Encode(out)
}

func configSet(v string) error {
	switch v {
	case "auto", "automatic":
		v = ""
	}
	if v != "" && !extract.ValidPiModel(v) {
		return fmt.Errorf("%q is not provider/model, as in anthropic/claude-haiku-5-5, or auto", v)
	}
	s := openStore()
	defer s.Close()
	return s.SetChoice(agent.Pi.ModelSetting, v)
}
