package server

import "testing"

func TestCandidateURLs(t *testing.T) {
	cases := []struct {
		in    string
		want  []string
	}{
		{
			"https://cdn.modrinth.com/data/AANobbMI/versions/RncWhTxD/sodium.jar",
			[]string{
				"https://mod.mcimirror.top/data/AANobbMI/versions/RncWhTxD/sodium.jar",
				"https://cdn.modrinth.com/data/AANobbMI/versions/RncWhTxD/sodium.jar",
			},
		},
		{
			"https://mediafilez.forgecdn.net/files/8456/972/jei.jar",
			[]string{
				"https://mod.mcimirror.top/files/8456/972/jei.jar",
				"https://mediafilez.forgecdn.net/files/8456/972/jei.jar",
			},
		},
		{
			"https://edge.forgecdn.net/files/8456/972/jei.jar",
			[]string{
				"https://mod.mcimirror.top/files/8456/972/jei.jar",
				"https://edge.forgecdn.net/files/8456/972/jei.jar",
			},
		},
		{
			"https://github.com/foo/bar/releases/x.jar",
			[]string{"https://github.com/foo/bar/releases/x.jar"},
		},
	}
	for _, c := range cases {
		got := candidateURLs(c.in)
		if len(got) != len(c.want) {
			t.Fatalf("candidateURLs(%q) = %v, want %v", c.in, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("candidateURLs(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}
