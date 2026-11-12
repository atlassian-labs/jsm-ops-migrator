package review

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/store"
)

// The review files, by name.
const (
	PlanFile               = "plan.json"
	PlanMarkdownFile       = "plan.md"
	DiffFile               = "diff.md"
	RiskQueueFile          = "risk-queue.md"
	AcceptanceTemplateFile = "acceptance-template.yaml"
	ConfirmationFile       = "confirmation.md"
	RunMarkdownFile        = "run.md"
	RunFile                = "run.json"
	MappingFile            = "mapping.csv"
)

// PlanDir is a plan version's folder, relative to artifacts/.
func PlanDir(version int) string { return filepath.Join("plans", fmt.Sprintf("v%d", version)) }

// RunDir is a run's folder, relative to artifacts/.
func RunDir(id string) string { return filepath.Join("runs", id) }

// WritePlanVersion writes p's review files to artifacts/plans/v<N>/ and
// returns the hash of its plan.json. prev is the version before, for the
// diff, or nil. The folder appears only once every file in it is written, so
// a crash never leaves a version half written, and an existing version is
// never replaced: once a plan is reviewed, its files stay as they were.
//
// The caller holds the workspace lock, as plan does, so a temporary folder
// left by an earlier write is from one that crashed, and is removed first.
func WritePlanVersion(ws *store.Workspace, p Plan, prev *Plan) (string, error) {
	data, err := Encode(p)
	if err != nil {
		return "", err
	}
	hash := Hash(data)
	if err := removeUncommitted(ws); err != nil {
		return "", fmt.Errorf("writing plan v%d: %w", p.Version, err)
	}
	files := []struct {
		name string
		data []byte
	}{
		{PlanFile, data},
		{PlanMarkdownFile, RenderPlan(p)},
		{DiffFile, RenderDiff(Compare(prev, p))},
		{RiskQueueFile, RenderRiskQueue(p)},
		{AcceptanceTemplateFile, AcceptanceTemplate(p, hash)},
	}
	tmp := filepath.Join("plans", fmt.Sprintf(".v%d.tmp-%s", p.Version, randomSuffix()))
	for _, f := range files {
		if err := ws.WriteArtifact(filepath.Join(tmp, f.name), f.data); err != nil {
			_ = ws.RemoveArtifactDir(tmp)
			return "", fmt.Errorf("writing plan v%d: %w", p.Version, err)
		}
	}
	if err := ws.CommitArtifactDir(tmp, PlanDir(p.Version)); err != nil {
		_ = ws.RemoveArtifactDir(tmp)
		return "", fmt.Errorf("writing plan v%d: %w", p.Version, err)
	}
	return hash, nil
}

var uncommittedRe = regexp.MustCompile(`^\.v\d+\.tmp-[0-9a-f]+$`)

// removeUncommitted removes the temporary folders of plan versions whose
// writing never reached its commit.
func removeUncommitted(ws *store.Workspace) error {
	entries, err := os.ReadDir(filepath.Join(ws.ArtifactsDir(), "plans"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() && uncommittedRe.MatchString(e.Name()) {
			if err := ws.RemoveArtifactDir(filepath.Join("plans", e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// ErrNoPlanVersion means a plan version has no files to go with a
// confirmation: its plan.json is missing.
var ErrNoPlanVersion = errors.New("the plan version has not been written")

// WriteConfirmation writes confirmation.md into the folder of the plan
// version it confirms. The version's plan.json must be there and be p's, and
// c must confirm exactly it (Confirmation.Check), so a record never names a
// plan nobody reviewed, and never creates a version's folder before the
// version is written.
func WriteConfirmation(ws *store.Workspace, c Confirmation, p Plan) error {
	onDisk, err := os.ReadFile(filepath.Join(ws.ArtifactsDir(), PlanDir(c.PlanVersion), PlanFile))
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("confirming plan v%d: %w", c.PlanVersion, ErrNoPlanVersion)
	}
	if err != nil {
		return err
	}
	data, err := Encode(p)
	if err != nil {
		return err
	}
	hash := Hash(onDisk)
	if Hash(data) != hash {
		return fmt.Errorf("confirming plan v%d: the plan given is not the one in its plan.json", c.PlanVersion)
	}
	if err := c.Check(p, hash); err != nil {
		return err
	}
	return ws.WriteArtifact(filepath.Join(PlanDir(c.PlanVersion), ConfirmationFile), RenderConfirmation(c, p))
}

// WriteRun writes the run record to artifacts/runs/<id>/ and returns the path
// of its run.md under artifacts/. Each file is replaced whole, and the record
// is rebuilt from the journal, so it is right again after a crash.
func WriteRun(ws *store.Workspace, r Run) (string, error) {
	data, err := EncodeRun(r)
	if err != nil {
		return "", err
	}
	mapping, err := MappingCSV(r)
	if err != nil {
		return "", err
	}
	dir := RunDir(r.ID)
	for _, f := range []struct {
		name string
		data []byte
	}{
		{RunFile, data},
		{MappingFile, mapping},
		{RunMarkdownFile, RenderRun(r)},
	} {
		if err := ws.WriteArtifact(filepath.Join(dir, f.name), f.data); err != nil {
			return "", fmt.Errorf("writing the record of run %s: %w", r.ID, err)
		}
	}
	return filepath.Join(dir, RunMarkdownFile), nil
}

func randomSuffix() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
