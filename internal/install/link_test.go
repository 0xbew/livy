package install

import (
	"os"
	"path/filepath"
	"testing"
)

// A file or link named livy that is not Livy's own must survive install and uninstall.
func TestLinkNeverTouchesWhatIsNotLivys(t *testing.T) {
	home := t.TempDir()
	t.Setenv("LIVY_LINK_DIR", filepath.Join(home, "bin"))
	bin := filepath.Join(home, ".livy", "bin", "livy")
	_ = os.MkdirAll(filepath.Dir(bin), 0o755)
	_ = os.MkdirAll(filepath.Join(home, "bin"), 0o755)
	link := filepath.Join(home, "bin", "livy")
	other := filepath.Join(home, "other-tool")
	_ = os.WriteFile(other, []byte("x"), 0o755)

	for name, setup := range map[string]func(){
		"regular file":    func() { _ = os.WriteFile(link, []byte("mine"), 0o755) },
		"foreign symlink": func() { _ = os.Symlink(other, link) },
	} {
		os.Remove(link)
		setup()
		if p, _, _ := installLink(home, bin, false); p != "" {
			t.Errorf("%s: install claimed %s", name, p)
		}
		if n := removeLink(link, bin, false); n.text != "" {
			t.Errorf("%s: uninstall removed it: %s", name, n.text)
		}
		if _, err := os.Lstat(link); err != nil {
			t.Errorf("%s: the file is gone: %v", name, err)
		}
	}
	os.Remove(link)
	if p, _, _ := installLink(home, bin, false); p != link {
		t.Fatalf("a free name was not linked: %q", p)
	}
	if n := removeLink(link, bin, false); n.text == "" {
		t.Error("Livy's own link was not removed")
	}
}
