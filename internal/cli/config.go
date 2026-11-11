package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"go.yaml.in/yaml/v3"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/store"
)

// envPrefix starts every environment variable the CLI reads.
const envPrefix = "JSM_OPS_MIGRATOR"

// Config holds the workspace settings in config.yaml. None of them is secret,
// so the operator can read and edit the file; credentials never go here.
//
// A task that needs another setting adds the field here, with its key in
// configKeys, in the same change.
type Config struct {
	Site               string `mapstructure:"site" yaml:"site,omitempty"`
	CloudID            string `mapstructure:"cloud_id" yaml:"cloud_id,omitempty"`
	OrgID              string `mapstructure:"org_id" yaml:"org_id,omitempty"`
	PagerDutySubdomain string `mapstructure:"pagerduty_subdomain" yaml:"pagerduty_subdomain,omitempty"`
	// PagerDutyRegion is where the PagerDuty account is hosted, us or eu. A
	// key works only in its account's region; login finds it and saves it
	// with the key.
	PagerDutyRegion string `mapstructure:"pagerduty_region" yaml:"pagerduty_region,omitempty"`
	// JSMPlan is the site's JSM plan as the operator states it; no API
	// reports it.
	JSMPlan string `mapstructure:"jsm_plan" yaml:"jsm_plan,omitempty"`
}

type configKey struct{ key, flag, usage string }

// configKeys bind each setting to JSM_OPS_MIGRATOR_<KEY> and to a flag.
var configKeys = []configKey{
	{"site", "site", "Atlassian site, for example acme.atlassian.net (overrides the workspace config)"},
	{"cloud_id", "cloud-id", "cloudId of the site (overrides the workspace config)"},
	{"org_id", "org-id", "Atlassian organization ID (overrides the workspace config)"},
	{"pagerduty_subdomain", "pagerduty-subdomain", "PagerDuty subdomain (overrides the workspace config)"},
	{"pagerduty_region", "pagerduty-region", "where the PagerDuty account is hosted: us or eu (overrides the workspace config; login finds it when not given)"},
	{"jsm_plan", "jsm-plan", "the site's JSM plan: free, standard, premium or enterprise (overrides the workspace config)"},
}

// loadConfig resolves the settings: the file, then the environment, then the
// flags, later sources winning. An unknown key in the file is an error, so a
// typo is never silently ignored.
func loadConfig(ws *store.Workspace, flags *pflag.FlagSet) (Config, error) {
	// One viper instance per call, never the global one, so in-process runs
	// never see each other's settings.
	v := viper.New()
	v.SetConfigType("yaml")
	// The file goes through LoadConfigFile, so a site saved without its
	// cloudId is dropped here too.
	saved, err := LoadConfigFile(ws)
	if err != nil {
		return Config{}, err
	}
	data, err := yaml.Marshal(saved)
	if err != nil {
		return Config{}, err
	}
	if err := v.ReadConfig(bytes.NewReader(data)); err != nil {
		return Config{}, fmt.Errorf("reading %s: %w", ws.ConfigPath(), err)
	}
	for _, k := range configKeys {
		if err := v.BindEnv(k.key, envPrefix+"_"+strings.ToUpper(k.key)); err != nil {
			return Config{}, err
		}
		if f := flags.Lookup(k.flag); f != nil {
			if err := v.BindPFlag(k.key, f); err != nil {
				return Config{}, err
			}
		}
	}
	var c Config
	if err := v.UnmarshalExact(&c); err != nil {
		return Config{}, fmt.Errorf("%s: %w", ws.ConfigPath(), err)
	}
	return c, nil
}

// givenSettings are the settings this run sets with a flag or an environment
// variable, as opposed to the saved ones. An empty variable counts as unset,
// as it does in loadConfig.
func givenSettings(flags *pflag.FlagSet) map[string]bool {
	out := map[string]bool{}
	for _, k := range configKeys {
		if os.Getenv(envPrefix+"_"+strings.ToUpper(k.key)) != "" {
			out[k.key] = true
		}
		if f := flags.Lookup(k.flag); f != nil && f.Changed {
			out[k.key] = true
		}
	}
	return out
}

// PagerDuty regions, as pagerduty_region names them.
const (
	regionUS = "us"
	regionEU = "eu"
)

// pagerDutyRegion checks a pagerduty_region value and returns it in the form
// the clients take. Empty means us, PagerDuty's default.
func pagerDutyRegion(v string) (string, error) {
	switch r := strings.ToLower(strings.TrimSpace(v)); r {
	case "", regionUS:
		return regionUS, nil
	case regionEU:
		return regionEU, nil
	}
	return "", fmt.Errorf("pagerduty_region %q: want %s or %s", v, regionUS, regionEU)
}

func otherRegion(r string) string {
	if r == regionEU {
		return regionUS
	}
	return regionEU
}

// LoadConfigFile returns only what config.yaml holds, without environment or
// flag overrides, for code that changes a setting and saves the file back.
//
// login saves a site only after it gave its cloudId, and saves both together.
// A site without one was never checked (typed into the file, or saved by an
// earlier version), so it counts as not saved: the menu does not show it and
// plan does not use it. The next save leaves it out of the file.
func LoadConfigFile(ws *store.Workspace) (Config, error) {
	data, err := os.ReadFile(ws.ConfigPath())
	if errors.Is(err, fs.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, err
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("%s: %w", ws.ConfigPath(), err)
	}
	if c.CloudID == "" {
		c.Site = ""
	}
	return c, nil
}

// SaveConfig writes c to config.yaml atomically with mode 0600. It writes only
// the given values, never the current run's environment or flag overrides.
func SaveConfig(ws *store.Workspace, c Config) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return ws.WriteFileAtomic(store.ConfigFile, data)
}
