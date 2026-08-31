package harness

import (
	"reflect"
	"testing"
)

func TestWSLGlobArgsPreserveInnerShellExpansion(t *testing.T) {
	pattern := "/home/user/.nvm/versions/node/*/bin"
	got := wslGlobArgs(pattern)
	want := []string{
		"sh",
		"-c",
		`for p in "\$@"; do [ -e "\$p" ] && printf "%s\n" "\$p"; done`,
		"--",
		pattern,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("wslGlobArgs() = %#v, want %#v", got, want)
	}
}

func TestWSLRunWithDirArgsPreservePATHAndArguments(t *testing.T) {
	args := []string{"/home/user/bin/codex", "login", "status"}
	got := wslRunWithDirArgs("/home/user/bin", args)
	want := []string{
		"sh",
		"-c",
		`export PATH='/home/user/bin':\$PATH; exec "\$@"`,
		"--",
		"/home/user/bin/codex",
		"login",
		"status",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("wslRunWithDirArgs() = %#v, want %#v", got, want)
	}
}
