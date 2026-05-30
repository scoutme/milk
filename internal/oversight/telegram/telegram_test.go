package telegram

import (
	"testing"
)

func TestEscMD(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"hello", "hello"},
		{"file.go", `file\.go`},
		{"path/to/file_name", `path/to/file\_name`},
		{"cmd: git push --force", `cmd: git push \-\-force`},
		{"1+1=2", `1\+1\=2`},
		{"(parens)", `\(parens\)`},
		{"*bold*", `\*bold\*`},
		{"`code`", "\\`code\\`"},
	}
	for _, tc := range cases {
		got := escMD(tc.in)
		if got != tc.want {
			t.Errorf("escMD(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
