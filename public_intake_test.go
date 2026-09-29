package switchboard

// Public-intake files (#435): the issue forms and SECURITY.md are read by outside reporters on the
// GitHub mirror, who cannot reach the private Gitea. A gitea.stump.rocks link in any of them is a
// dead link for exactly the audience the file exists for, and it only shows up once the mirror has
// published it. The chooser split is also load-bearing: GitHub takes .github/ISSUE_TEMPLATE/config.yml
// (forms only), while Gitea reads .gitea/ISSUE_TEMPLATE/config.yaml first and keeps blank issues open
// on the canonical tracker. Deleting the Gitea file silently closes blank issues there too.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublicIntakeFilesHaveNoPrivateForgeLinks(t *testing.T) {
	files, err := filepath.Glob(".github/ISSUE_TEMPLATE/*.yml")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 3 {
		t.Fatalf("expected the bug form, the feature form and config.yml under .github/ISSUE_TEMPLATE, found %v", files)
	}
	files = append(files, "SECURITY.md")
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if strings.Contains(string(raw), "gitea.stump.rocks") {
			t.Errorf("%s links the private Gitea; public reporters on the GitHub mirror cannot reach it", path)
		}
	}
}

func TestIssueChooserConfigPerForge(t *testing.T) {
	for path, want := range map[string]string{
		".github/ISSUE_TEMPLATE/config.yml": "blank_issues_enabled: false",
		".gitea/ISSUE_TEMPLATE/config.yaml": "blank_issues_enabled: true",
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !strings.Contains(string(raw), "\n"+want+"\n") {
			t.Errorf("%s: want a top-level %q line", path, want)
		}
	}
}
