package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/preflight"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/store"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/tui"
)

var credentialHelp = map[credentials.Kind]string{
	credentials.PagerDutyKey:   "A read-only REST API key from your PagerDuty account's API Access Keys.",
	credentials.AtlassianEmail: "The email of your Atlassian account. The account must be an admin of the organization.",
	credentials.AtlassianToken: "Create one at https://id.atlassian.com/manage-profile/security/api-tokens.",
	credentials.OrgAPIKey:      "Create one at https://admin.atlassian.com, under Settings > API keys.",
}

// checkedBy maps each login check to the credentials it proves, so only a
// credential whose check passed is stored.
var checkedBy = map[preflight.ID][]credentials.Kind{
	preflight.PagerDutyKey:   {credentials.PagerDutyKey},
	preflight.AtlassianToken: {credentials.AtlassianEmail, credentials.AtlassianToken},
	preflight.OrgKey:         {credentials.OrgAPIKey},
}

// runLogin collects the credentials, the site and the organization, checks
// them, and stores the credentials in the workspace's vault and the site and
// organization in config.yaml. Only a credential whose check passed is stored:
// one that was rejected or could not be checked is left out, and a stored one
// it would replace stays.
func runLogin(ctx context.Context, env *Env, f LoginFlags) error {
	if f.Clear {
		return clearCredentials(ctx, env)
	}
	st, err := env.OpenStore(ctx, store.OpenCreate)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	vault := env.vault(st)
	stored, err := vault.Load()
	if err != nil {
		return err
	}
	fileCfg, err := LoadConfigFile(env.Workspace)
	if err != nil {
		return err
	}

	given, sources, err := givenCredentials(env, f)
	if err != nil {
		return err
	}
	// With --stdin the input is the credentials, so there is nothing left to
	// prompt with.
	interactive := env.Interactive && !f.Stdin
	fresh, site, orgID := given, env.Config.Site, env.Config.OrgID
	if interactive {
		if fresh, site, orgID, err = askLogin(ctx, env, stored, given, site, orgID); err != nil {
			return err
		}
	}
	creds := stored.Merge(fresh)
	if site, err = normalizeSite(site); err != nil {
		return fmt.Errorf("--site: %w", err)
	}

	clients, err := env.Clients(creds)
	if err != nil {
		return err
	}
	if orgID == "" {
		if orgID, err = chooseOrg(ctx, env, interactive, clients, creds); err != nil {
			return err
		}
	}
	report, err := preflight.Run(ctx, clients, preflight.Input{
		Credentials: creds, Sources: sources, Site: site, OrgID: orgID, Checks: preflight.Login, Now: env.Now,
	})
	if err != nil {
		return err
	}

	var keep, rejected, unchecked []credentials.Kind
	for _, k := range credentials.Kinds {
		switch checkStatus(report, k) {
		case preflight.Failed:
			rejected = append(rejected, k)
		case preflight.NotChecked:
			unchecked = append(unchecked, k)
		default:
			keep = append(keep, k)
		}
	}
	save := creds.Only(keep...).Merge(stored.Only(rejected...)).Merge(stored.Only(unchecked...))
	if err := vault.Save(save); err != nil {
		return err
	}

	// Like the credentials, the site and organization are saved only when
	// their check passed. A different one whose check did not pass also
	// removes the saved one, so a later plan cannot use a site or
	// organization the operator has moved away from. The same one stays.
	cfg, notSaved := fileCfg, savedConfig{}
	var removed []string
	if site != "" {
		if c, _ := report.Find(preflight.Site); c.Status == preflight.OK {
			cfg.Site, cfg.CloudID = site, report.CloudID
		} else if site != fileCfg.Site {
			notSaved.Site = site
			if fileCfg.Site != "" {
				cfg.Site, cfg.CloudID = "", ""
				removed = append(removed, "site")
			}
		}
	}
	if orgID != "" {
		if c, _ := report.Find(preflight.OrgKey); c.Status == preflight.OK {
			cfg.OrgID = orgID
		} else if orgID != fileCfg.OrgID {
			notSaved.OrgID = orgID
			if fileCfg.OrgID != "" {
				cfg.OrgID = ""
				removed = append(removed, "organization")
			}
		}
	}
	if cfg != fileCfg {
		if err := SaveConfig(env.Workspace, cfg); err != nil {
			return err
		}
	}

	res := loginResult{
		Checks:    report.Checks,
		Stored:    kindsIn(save),
		StoredIn:  vault.Where(),
		Rejected:  kindsIn(fresh.Only(rejected...)),
		Unchecked: kindsIn(fresh.Only(unchecked...)),
		Config:    savedConfig{Site: cfg.Site, CloudID: cfg.CloudID, OrgID: cfg.OrgID},
		NotSaved:  notSaved,
		storedNow: kindsIn(fresh.Only(keep...)),
		savedSite: cfg.Site != "" && (cfg.Site != fileCfg.Site || cfg.CloudID != fileCfg.CloudID),
		savedOrg:  cfg.OrgID != "" && cfg.OrgID != fileCfg.OrgID,
		removed:   removed,
	}
	if err := writeLoginResult(env, res); err != nil {
		return err
	}
	env.summarize(loginSummary(res))
	return checksError("login", report)
}

// checkStatus is the status of the check that proves credential k. A
// credential no check proves counts as not checked, so it is never stored.
func checkStatus(report preflight.Report, k credentials.Kind) preflight.Status {
	for id, kinds := range checkedBy {
		if slices.Contains(kinds, k) {
			if c, ok := report.Find(id); ok {
				return c.Status
			}
		}
	}
	return preflight.NotChecked
}

func kindsIn(s credentials.Set) []credentials.Kind {
	var out []credentials.Kind
	for _, k := range credentials.Kinds {
		if s.Has(k) {
			out = append(out, k)
		}
	}
	return out
}

// givenCredentials are the ones set in the environment and, with --stdin,
// read from standard input, which wins.
func givenCredentials(env *Env, f LoginFlags) (credentials.Set, map[credentials.Kind]string, error) {
	given, kinds := credentials.FromEnv(os.Getenv)
	sources := map[credentials.Kind]string{}
	for _, k := range kinds {
		sources[k] = "from " + k.EnvVar()
	}
	if !f.Stdin {
		return given, sources, nil
	}
	fromStdin, err := credentials.ParseInput(env.Stdin)
	if err != nil {
		return credentials.Set{}, nil, fmt.Errorf("--stdin: %w", err)
	}
	for _, k := range kindsIn(fromStdin) {
		sources[k] = "from standard input"
	}
	return given.Merge(fromStdin), sources, nil
}

// askLogin asks, on one page, for the site, the organization and each
// credential the environment did not give, and returns the given and answered
// credentials. An empty answer keeps the stored credential.
func askLogin(ctx context.Context, env *Env, stored, given credentials.Set, site, orgID string) (credentials.Set, string, string, error) {
	var qs []tui.Question
	// The credential each question asks for, or one of these for a setting.
	const siteQ, orgQ credentials.Kind = "", "org"
	var kinds []credentials.Kind
	add := func(k credentials.Kind) {
		if given.Has(k) {
			return
		}
		q := tui.Question{Title: capitalize(k.Label()), Description: credentialHelp[k], Masked: k != credentials.AtlassianEmail}
		if stored.Has(k) {
			q.Description += "\nLeave it empty to keep the stored one."
		}
		if k == credentials.AtlassianEmail {
			q.Validate = validateEmail
		}
		qs, kinds = append(qs, q), append(kinds, k)
	}
	add(credentials.PagerDutyKey)
	qs = append(qs, tui.Question{
		Title:       "Atlassian site",
		Description: "The site to migrate into, for example acme.atlassian.net.",
		Value:       site,
		Validate:    func(v string) error { _, err := normalizeSite(v); return err },
	})
	kinds = append(kinds, siteQ)
	add(credentials.AtlassianEmail)
	add(credentials.AtlassianToken)
	add(credentials.OrgAPIKey)
	qs = append(qs, tui.Question{
		Title: "Organization ID",
		Description: "In https://admin.atlassian.com, the ID after /o/ in the address.\n" +
			"Leave it empty to use the organization the org API key manages.",
		Value: orgID,
	})
	kinds = append(kinds, orgQ)

	answers, err := env.Ask(ctx, "Log in to PagerDuty and Atlassian", qs)
	if err != nil {
		return credentials.Set{}, "", "", err
	}
	answered := credentials.Set{}
	for i, k := range kinds {
		switch k {
		case siteQ:
			site = answers[i]
		case orgQ:
			orgID = strings.TrimSpace(answers[i])
		default:
			answered = answered.With(k, answers[i])
		}
	}
	return given.Merge(answered), site, orgID, nil
}

// chooseOrg picks the organization when none was given: the org API key's
// only one, or, in a terminal, the one the operator chooses among several.
// The list exists only once the key is known, so choosing among several is the
// one question that cannot be on the login page. Otherwise the organization
// stays unset, and the org API key check says so.
func chooseOrg(ctx context.Context, env *Env, interactive bool, clients preflight.Clients, creds credentials.Set) (string, error) {
	var orgs []preflight.Org
	if creds.Has(credentials.OrgAPIKey) {
		// A failure here shows up in the org API key check.
		orgs, _ = clients.Admin.Orgs(ctx)
	}
	switch {
	case len(orgs) == 1:
		return orgs[0].ID, nil
	case len(orgs) > 1 && interactive:
		options := make([]tui.Option, len(orgs))
		for i, o := range orgs {
			options[i] = tui.Option{Key: o.ID, Label: fmt.Sprintf("%s (%s)", o.Name, o.ID)}
		}
		return env.Choose(ctx, "Organization", "The org API key can manage these organizations. Which one owns the site?", options, "")
	}
	return "", nil
}

// clearCredentials removes the stored credentials without the passphrase or
// the workspace's key, so it works even when those are lost.
func clearCredentials(ctx context.Context, env *Env) error {
	info, err := store.ReadInfo(ctx, env.Workspace)
	if err != nil {
		return err
	}
	where := displayPath(env.Workspace.Dir())
	if info.WorkspaceID == "" {
		_, err := fmt.Fprintf(env.Stdout, "No credentials are stored in %s.\n", where)
		return err
	}
	var v credentials.Vault
	if info.KeyProvider == credentials.PassphraseProviderID {
		v = credentials.NewFileVault(env.Workspace, nil)
	} else {
		v = credentials.NewKeychainVault(env.keychain, info.WorkspaceID)
	}
	if err := v.Clear(); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(env.Stdout, "Removed the stored credentials of %s from %s.\n", where, v.Where()); err != nil {
		return err
	}
	if _, kinds := credentials.FromEnv(os.Getenv); len(kinds) > 0 {
		var names []string
		for _, k := range kinds {
			names = append(names, k.EnvVar())
		}
		_, err := fmt.Fprintf(env.Stdout, "Still set in the environment: %s.\n", strings.Join(names, ", "))
		return err
	}
	return nil
}

var hostRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

// normalizeSite turns what an operator types, such as "acme",
// "https://acme.atlassian.net/jira" or "ACME.atlassian.net", into the site's
// host name. A name without a dot is an atlassian.net site.
func normalizeSite(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "", nil
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.User != nil || u.Port() != "" || u.Hostname() == "" {
		return "", errors.New("give the site's address, for example acme.atlassian.net")
	}
	host := u.Hostname()
	if !strings.Contains(host, ".") {
		host += ".atlassian.net"
	}
	if !hostRe.MatchString(host) {
		return "", errors.New("give the site's address, for example acme.atlassian.net")
	}
	return host, nil
}

func validateEmail(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	name, domain, ok := strings.Cut(v, "@")
	if !ok || name == "" || !strings.Contains(domain, ".") || strings.ContainsAny(v, " \t") {
		return errors.New("give the email of your Atlassian account")
	}
	return nil
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
