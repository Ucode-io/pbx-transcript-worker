package main

import "testing"

func sample() []Track {
	return []Track{
		{Speaker: "operator", Text: "privet visper", Segments: []Segment{
			{From: 0, To: 1000, Text: "privet"},
			{From: 1000, To: 2000, Text: "visper"},
		}},
		{Speaker: "client", Text: "yaxshi rahmat"},
	}
}

func TestApplyPolishedMapsLinesBackAndRebuildsTrackText(t *testing.T) {
	got := applyPolished(sample(), []string{"привет", "Whisper", "yaxshi, rahmat"})

	if got[0].Segments[0].Text != "привет" || got[0].Segments[1].Text != "Whisper" {
		t.Fatalf("segments not polished: %+v", got[0].Segments)
	}
	if got[0].Text != "привет Whisper" {
		t.Fatalf("track text not rebuilt: %q", got[0].Text)
	}
	if got[0].Segments[1].From != 1000 || got[0].Segments[1].To != 2000 {
		t.Fatalf("timecodes changed: %+v", got[0].Segments[1])
	}
	// A track without segments is one line of its own.
	if got[1].Text != "yaxshi, rahmat" {
		t.Fatalf("segmentless track not polished: %q", got[1].Text)
	}
}

func TestApplyPolishedKeepsRawOnLengthMismatch(t *testing.T) {
	got := applyPolished(sample(), []string{"привет"})

	if got[0].Segments[1].Text != "visper" || got[1].Text != "yaxshi rahmat" {
		t.Fatalf("partially applied a bad answer: %+v", got)
	}
}
