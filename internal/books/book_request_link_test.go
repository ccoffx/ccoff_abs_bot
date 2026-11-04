package books

import "testing"

func TestBookRequestRequiresAlbumHomepage(t *testing.T) {
	for _, tc := range []struct {
		url   string
		valid bool
	}{
		{"https://www.ximalaya.com/album/12345678", true},
		{"https://m.ximalaya.com/album/12345678.html", true},
		{"https://ximalaya.com/album/12345678/?share=1", true},
		{"https://www.ximalaya.com/sound/12345678", false},
		{"https://m.ximalaya.com/album/12345678/sound/222", false},
		{"https://www.ximalaya.com/album/", false},
		{"https://www.ximalaya.com/album/not-an-id", false},
		{"https://www.ximalaya.com/explore/12345678", false},
		{"https://xima.tv/1_abcd", false},
		{"https://www.ximalaya.com:8443/album/12345678", false},
		{"https://www.ximalaya.com.evil.example/album/12345678", false},
		{"https://name:pass@www.ximalaya.com/album/12345678", false},
		{"http://www.ximalaya.com/album/12345678", false},
		{"https://www.ximalaya.com/album/12345678\n/sound/1", false},
	} {
		t.Run(tc.url, func(t *testing.T) {
			_, got := ValidateXmlyLink(tc.url)
			if got != tc.valid {
				t.Fatalf("accepted=%t want=%t", got, tc.valid)
			}
		})
	}
}

func TestXmlyAlbumIDIsNotUsedAsTitleHint(t *testing.T) {
	if hint := extractXmlyTitleHint("https://m.ximalaya.com/album/12345678.html"); hint != "" {
		t.Fatalf("album ID became title hint: %q", hint)
	}
	if hint := extractXmlyTitleHint("https://www.ximalaya.com/album/12345678?title=Example"); hint != "Example" {
		t.Fatalf("explicit title hint lost: %q", hint)
	}
}
