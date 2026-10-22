package execution

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestMaExecutionSourcesDoNotShellOutOrPrune(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("source path is unavailable")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	roots := []string{
		filepath.Join(root, "internal", "execution"),
		filepath.Join(root, "cmd", "oma-broker"),
		filepath.Join(root, "cmd", "oma-proxy"),
	}
	for _, dir := range roots {
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			if strings.HasSuffix(path, "_test.go") {
				return nil
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			text := string(raw)
			if strings.Contains(text, "os/exec") || strings.Contains(text, "docker prune") || strings.Contains(text, "exec.Command") {
				t.Errorf("%s shells out or prunes docker", path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
