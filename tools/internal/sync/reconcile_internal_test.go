package sync

import (
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/source"
)

func TestReconcileResultRenderingIsDeterministic(t *testing.T) {
	result := ReconcileResult{
		Schema: ReconcileSchema,
		Actions: []ReconcileAction{{
			Kind: "checkpoint", SourceTag: "v1.36.2", DestinationTag: "v0.36.2",
			PlanHash:    "sha256:" + strings.Repeat("a", 64),
			StateCommit: strings.Repeat("b", 40), Progress: "refs/soapbox/progress/v0.36.2",
			Done: 100, Total: 200, Applied: true,
		}},
		BudgetExhausted: true,
	}
	first, err := result.JSON()
	if err != nil {
		t.Fatalf("render JSON: %v", err)
	}
	second, err := result.JSON()
	if err != nil {
		t.Fatalf("render JSON again: %v", err)
	}
	if string(first) != string(second) || first[len(first)-1] != '\n' {
		t.Fatalf("JSON is not stable newline-terminated bytes:\n%s\n%s", first, second)
	}
	for _, forbidden := range []string{"/tmp/", "https://", "SOAPBOX_GITHUB_TOKEN"} {
		if strings.Contains(string(first), forbidden) || strings.Contains(result.Text(), forbidden) {
			t.Errorf("report contains forbidden local/credential material %q", forbidden)
		}
	}
	for _, want := range []string{"checkpoint", "v1.36.2 -> v0.36.2", "100/200", "workflow budget"} {
		if !strings.Contains(result.Text(), want) {
			t.Errorf("text report does not contain %q:\n%s", want, result.Text())
		}
	}
}

func TestReconcileResultReportsFixedPointWriteVerification(t *testing.T) {
	t.Parallel()

	result := ReconcileResult{
		Schema: ReconcileSchema, FixedPoint: true, WriteVerified: true,
		Actions: []ReconcileAction{},
	}
	encoded, err := result.JSON()
	if err != nil {
		t.Fatalf("render JSON: %v", err)
	}
	if !strings.Contains(string(encoded), `"writeVerified": true`) {
		t.Errorf("JSON does not report write verification:\n%s", encoded)
	}
	if !strings.Contains(result.Text(), "leased no-op push verified") {
		t.Errorf("text does not report write verification:\n%s", result.Text())
	}
}

func TestCheckReconcileOptions(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	git, err := gitcli.New(ctx, gitcli.Options{Dir: dir, Inherit: []string{"PATH"}})
	if err != nil {
		t.Fatalf("new Git runner: %v", err)
	}
	if err := git.InitRepository(ctx, "main"); err != nil {
		t.Fatalf("init repository: %v", err)
	}
	cfg := &config.Config{Publication: config.Publication{Mode: config.PublicationModeManual}}
	cache := &source.Cache{}
	tests := []struct {
		name string
		opts ReconcileOptions
		want string
	}{
		{name: "no profile", want: "profile"},
		{name: "no source cache", opts: ReconcileOptions{Config: cfg}, want: "source cache"},
		{
			name: "local runner allows lazy fetch",
			opts: ReconcileOptions{Config: cfg, SourceCache: cache, LocalGit: git},
			want: "lazy fetching disabled",
		},
		{
			name: "automatic manual profile",
			opts: ReconcileOptions{
				Config: cfg, SourceCache: cache, LocalGit: git.WithNoLazyFetch(),
				Destination: Destination{Git: git}, Automatic: true,
			},
			want: "publication mode",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := checkReconcileOptions(test.opts)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("check = %v, want %q", err, test.want)
			}
		})
	}
}
