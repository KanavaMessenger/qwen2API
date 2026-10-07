package main

import (
	cryptorand "crypto/rand"
	"fmt"
	"math/big"
	"strings"
)

// Upstream chat titles used to be "api_<unix timestamp>", a constant, easily
// fingerprinted pattern. randomChatTitle builds a varied, human-looking title
// from several shapes and word lists instead.

var (
	titleAdjectives = []string{
		"quiet", "bright", "curious", "gentle", "swift", "golden", "silver", "hidden", "lazy", "brave",
		"calm", "clever", "cosmic", "crimson", "dusty", "eager", "faded", "frosty", "happy", "hollow",
		"icy", "jolly", "keen", "lucky", "mellow", "misty", "noble", "odd", "polite", "rapid",
		"rustic", "shiny", "silent", "sleepy", "smooth", "stormy", "sunny", "tiny", "vivid", "wild",
		"wise", "witty", "young", "amber", "azure", "breezy", "cheerful", "daring", "electric", "fuzzy",
	}
	titleNouns = []string{
		"harbor", "lantern", "falcon", "meadow", "compass", "river", "orchard", "pebble", "comet", "canyon",
		"violin", "anchor", "bridge", "candle", "cedar", "cloud", "coral", "desert", "ember", "feather",
		"forest", "garden", "glacier", "island", "jungle", "kettle", "lagoon", "maple", "mirror", "moon",
		"mountain", "otter", "panda", "piano", "planet", "prairie", "quartz", "raven", "reef", "ridge",
		"saddle", "signal", "sparrow", "summit", "thunder", "tulip", "valley", "willow", "window", "zephyr",
		"atlas", "beacon", "cabin", "delta", "echo", "fjord", "grove", "haven", "jasper", "lotus",
	}
	titleTopics = []string{
		"travel plans", "recipe ideas", "reading list", "project outline", "weekend plans", "study notes",
		"budget draft", "workout routine", "gift ideas", "meeting prep", "trip checklist", "book summary",
		"garden plan", "movie night", "language practice", "code review", "design sketch", "meal prep",
		"home repair", "music playlist", "career advice", "product idea", "event schedule", "photo editing",
	}
	titlePrefixes = []string{"Notes on", "Ideas for", "Thoughts about", "Quick question:", "Help with", "Draft:", "Chat about", "Plan for"}
)

func titleRandInt(n int) int {
	if n <= 1 {
		return 0
	}
	v, err := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0
	}
	return int(v.Int64())
}

func titlePick(list []string) string { return list[titleRandInt(len(list))] }

func titleCap(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// randomChatTitle returns a short, varied conversation title.
func randomChatTitle() string {
	var title string
	switch titleRandInt(7) {
	case 0:
		title = titleCap(titlePick(titleAdjectives)) + " " + titleCap(titlePick(titleNouns))
	case 1:
		title = titleCap(titlePick(titleNouns)) + " " + titlePick(titleNouns)
	case 2:
		title = titleCap(titlePick(titleTopics))
	case 3:
		title = titlePick(titlePrefixes) + " " + titlePick(titleTopics)
	case 4:
		title = titleCap(titlePick(titleAdjectives)) + " " + titlePick(titleNouns) + fmt.Sprintf(" %d", 2+titleRandInt(98))
	case 5:
		title = titlePick(titleAdjectives) + " " + titlePick(titleNouns)
	default:
		title = titleCap(titlePick(titleTopics)) + " #" + fmt.Sprintf("%d", 1+titleRandInt(999))
	}
	return title
}
