package web

import "testing"

func TestCodeText(t *testing.T) {
	for in, want := range map[string]string{
		"plain <b> & “quotes”":            "plain &lt;b&gt; &amp; “quotes”",
		"add `--retry-connrefused` to it": `add <code class="token">--retry-connrefused</code> to it`,
		"`a` and `<b>`":                   `<code class="token">a</code> and <code class="token">&lt;b&gt;</code>`,
		"one ` only":                      "one ` only",
		"`a` and ` b":                     `<code class="token">a</code> and ` + "` b",
		"":                                "",
	} {
		if got := string(codeText(in)); got != want {
			t.Errorf("codeText(%q) = %q, want %q", in, got, want)
		}
	}
}
