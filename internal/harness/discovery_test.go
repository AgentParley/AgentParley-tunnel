package harness

import "testing"

func TestParseMarkerLine(t *testing.T) {
	cases := []struct {
		name   string
		output string
		marker string
		want   string
	}{
		{
			name:   "marker on its own line",
			output: "__AP_CMD__/home/kasper/.npm-global/bin/codex",
			marker: cmdMarker,
			want:   "/home/kasper/.npm-global/bin/codex",
		},
		{
			name:   "rc-file noise before and after the marker",
			output: "Welcome to fastfetch!\nsome banner text\n\n__AP_CMD__/usr/local/bin/claude\ntrailing junk\n",
			marker: cmdMarker,
			want:   "/usr/local/bin/claude",
		},
		{
			name:   "picks the requested marker, not the other one",
			output: "__AP_CMD__/usr/bin/codex\n__AP_PATH__/usr/bin:/bin",
			marker: pathMarker,
			want:   "/usr/bin:/bin",
		},
		{
			name:   "empty command from a shell that has nothing",
			output: "__AP_CMD__\n__AP_PATH__/usr/bin:/bin",
			marker: cmdMarker,
			want:   "",
		},
		{
			name:   "marker absent entirely",
			output: "no markers here at all\n",
			marker: cmdMarker,
			want:   "",
		},
		{
			name:   "carriage returns from a shell that emits CRLF",
			output: "__AP_CMD__/usr/bin/codex\r\n",
			marker: cmdMarker,
			want:   "/usr/bin/codex",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := parseMarkerLine(testCase.output, testCase.marker)
			if got != testCase.want {
				t.Errorf("parseMarkerLine(%q, %q) = %q, want %q", testCase.output, testCase.marker, got, testCase.want)
			}
		})
	}
}
