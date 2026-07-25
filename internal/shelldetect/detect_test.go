package shelldetect

import "testing"

func TestIsShellCommand(t *testing.T) {
	truePositives := []struct {
		input string
	}{
		{"git status"},
		{"git status -s"},
		{"ls -la /tmp"},
		{"ls"},
		{"cat foo.txt"},
		{"cat foo | grep bar"},
		{"grep -r 'TODO' ./src"},
		{"docker ps"},
		{"kubectl get pods"},
		{"go build ./..."},
		{"npm install"},
		{"make test"},
		{"curl http://example.com"},
		{"echo hello"},
		{"find . -name '*.go'"},
		{"git log --oneline -10"},
		{"ps aux"},
		{"df -h"},
		{"pwd"},
		{"touch newfile.txt"},
		{"rm -rf /tmp/test"},
		{"mkdir -p /tmp/dir"},
		{"tar -xzf archive.tar.gz"},
		{"sudo apt-get update"},
		{"ssh user@host"},
		{"python3 script.py"},
		{"pip install requests"},
		{"which git"},
		{"which python3"},
	}

	for _, tt := range truePositives {
		cmd, ok := IsShellCommand(tt.input)
		if !ok {
			t.Errorf("IsShellCommand(%q) = false, want true", tt.input)
		}
		if ok && cmd != tt.input {
			t.Errorf("IsShellCommand(%q) cmd = %q, want %q", tt.input, cmd, tt.input)
		}
	}

	falseNegatives := []struct {
		input string
	}{
		{"what does git status show?"},
		{"explain the output"},
		{"how do I use git?"},
		{"why is my code failing"},
		{"can you help me with this"},
		{"describe what ls does"},
		{"this is a very long sentence that should definitely not be treated as a shell command"},
		{"list all the files in the directory and explain each one"},
		{"I want to understand how docker works"},
		{"what is the difference between cat and less?"},
		{"please write a function that sorts"},
		// NL openers
		{"should I use git rebase or merge?"},
		{"could you explain what kubectl does?"},
		{"help me understand docker"},
		// Ambiguous but caught by NL word heuristic
		{"git explaining merge conflicts"},
		// NL connector words after a known binary
		{"go to main"},
		{"git push to origin"},
		{"cd into the project"},
		{"make a new branch"},
		// Empty / question mark
		{""},
		{"git status?"},
		// Unknown binary
		{"foobar --help"},
		{"unknowncmd arg1 arg2"},
	}

	for _, tt := range falseNegatives {
		_, ok := IsShellCommand(tt.input)
		if ok {
			t.Errorf("IsShellCommand(%q) = true, want false", tt.input)
		}
	}
}

func TestFirstToken(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"git status", "git"},
		{"ls -la", "ls"},
		{"  echo hello  ", "echo"},
		{"", ""},
		{"GIT status", "git"},
	}
	for _, tt := range tests {
		got := FirstToken(tt.input)
		if got != tt.want {
			t.Errorf("FirstToken(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestIsShellCommand_AllowListBypasses(t *testing.T) {
	// Kept for historical coverage; now delegates to HasAllowedPrefix.
	allowList := []string{"ls", "git"}
	if !HasAllowedPrefix("ls -la", allowList) {
		t.Error("HasAllowedPrefix: expected match for single-token entry 'ls'")
	}
}

func TestHasAllowedPrefix(t *testing.T) {
	cases := []struct {
		cmd   string
		allow []string
		want  bool
	}{
		// Single-token entry matches any subcommand.
		{"git status", []string{"git"}, true},
		{"git commit -m msg", []string{"git"}, true},
		// Multi-token entry matches exact prefix.
		{"go test ./...", []string{"go test"}, true},
		{"go build ./...", []string{"go test"}, false},
		{"go mod tidy", []string{"go mod"}, true},
		{"go mod tidy", []string{"go test", "go mod"}, true},
		// Multi-token entry does not match shorter command.
		{"go", []string{"go test"}, false},
		// Case-insensitive.
		{"Go Test ./...", []string{"go test"}, true},
		// No match.
		{"rm -rf /", []string{"ls", "git", "go test"}, false},
		// Empty allow-list.
		{"ls", []string{}, false},
	}
	for _, c := range cases {
		got := HasAllowedPrefix(c.cmd, c.allow)
		if got != c.want {
			t.Errorf("HasAllowedPrefix(%q, %v) = %v, want %v", c.cmd, c.allow, got, c.want)
		}
	}
}
