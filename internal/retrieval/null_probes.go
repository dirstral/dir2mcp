package retrieval

// The null probe set (SPEC §9.4.3, spec 0.76.0; issue #1081).
//
// These questions are about everyday subjects that no knowledge corpus is
// expected to answer. Embedded against the corpus, the top cosine each one
// reaches is the similarity an UNRELATED question scores with this embedder on
// this corpus. The 90th percentile of those readings is the null baseline the
// cosine evidence threshold is derived from (see evidence_baseline.go).
//
// The set is fixed and versioned, never drawn from the corpus, and spread over
// distinct subjects so that a corpus which happens to cover a few of them (a
// cookbook, a travel guide) moves the median little and the p90 not at all:
// measured on the benchmark corpus, three on-topic probes moved the p90 by
// 0.000 while the maximum went from 0.548 to 0.75. Changing a probe changes
// every cached baseline, so a change here MUST bump nullProbeSetVersion.
const nullProbeSetVersion = "v1"

var nullProbes = []string{
	"What time is it right now?",
	"How do I boil an egg?",
	"Where did I leave my keys?",
	"What will the weather be like tomorrow?",
	"How many steps should I walk each day?",
	"What should I cook for dinner tonight?",
	"How do I tie a necktie?",
	"When does the next bus leave?",
	"How do I change a flat tire?",
	"What is a good name for a cat?",
	"How many hours of sleep do I need?",
	"How do I fold a fitted sheet?",
	"How do I remove a coffee stain from a shirt?",
	"Which shoes go with a blue suit?",
	"How much water should I drink in a day?",
	"How do I reset a forgotten password?",
	"What is a good birthday gift for a friend?",
	"How often should I water a cactus?",
	"When should I plant tomatoes?",
	"How do I pack a suitcase for a week?",
	"What is the best way to learn to swim?",
	"How do I write a thank-you note?",
	"How do I stop hiccups?",
	"How do I park a car in a tight space?",
	"What is a good stretch after a run?",
	"How do I split a restaurant bill fairly?",
	"How can I sleep on a long flight?",
	"How do I clean a window without streaks?",
	"What snacks are good for a long drive?",
	"How do I calm a crying baby?",
	"How do I sharpen a kitchen knife?",
	"What is a simple recipe for pancakes?",
}
