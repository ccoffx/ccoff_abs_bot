package storage

import "testing"

func TestServerLinesMarkdownBodyMakesEachURLCopyable(t *testing.T) {
	got := ServerLinesMarkdownBody("主_线路：https://one.example/path?q=a_b\n备用线路：https://two.example/。")
	want := "主\\_线路：\x02https://one.example/path?q=a_b\x03\n备用线路：\x02https://two.example/\x03。"
	if got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}
