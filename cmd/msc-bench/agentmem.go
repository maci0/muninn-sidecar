package main

import "fmt"

// genAgentMem builds a corpus that mirrors msc's ACTUAL use case: an AI coding
// agent storing project decisions/config/ownership, then recalling them later —
// rather than Wikipedia QA. Each memory has a globally-unique coined subject
// (service/module/API/environment name) so a question about it maps to exactly
// one memory; the answer is a short distinctive span in that memory.
//
// Returns items (to seed) and probes carrying the gold answer (for msc-qa via a
// dumped QA file). This is the most realistic regime for the sidecar.
func genAgentMem(n, nAbsent int) ([]item, []probe, []probe) {
	dbs := []string{"PostgreSQL", "Redis", "DynamoDB", "ClickHouse", "Cassandra", "SQLite", "MongoDB", "CockroachDB"}
	people := []string{"Priya", "Marcus", "Lena", "Toshiro", "Amara", "Diego", "Freya", "Omar"}
	tools := []string{"ArgoCD", "Spinnaker", "GitHub Actions", "Jenkins", "Flux", "Drone"}
	triggers := []string{"a green main build", "a signed release tag", "manual approval", "a nightly cron"}

	mk := func(i int) (item, probe) {
		x := coinName(i)
		switch i % 4 {
		case 0:
			db := dbs[i%len(dbs)]
			return item{Concept: "svc-" + x, Content: fmt.Sprintf("The %s service stores its primary data in %s.", x, db)},
				probe{Query: fmt.Sprintf("Which datastore does the %s service use?", x), Gold: "svc-" + x, Answer: db, Present: true}
		case 1:
			p := people[i%len(people)]
			return item{Concept: "mod-" + x, Content: fmt.Sprintf("%s owns the %s module; route all reviews for it to them.", p, x)},
				probe{Query: fmt.Sprintf("Who owns the %s module?", x), Gold: "mod-" + x, Answer: p, Present: true}
		case 2:
			rl := 50 + (i%20)*25
			return item{Concept: "api-" + x, Content: fmt.Sprintf("The %s API enforces a rate limit of %d requests per minute per token.", x, rl)},
				probe{Query: fmt.Sprintf("What is the rate limit of the %s API?", x), Gold: "api-" + x, Answer: fmt.Sprintf("%d", rl), Present: true}
		default:
			t := tools[i%len(tools)]
			return item{Concept: "env-" + x, Content: fmt.Sprintf("Deploys to the %s environment run via %s, triggered on %s.", x, t, triggers[i%len(triggers)])},
				probe{Query: fmt.Sprintf("What tool deploys to the %s environment?", x), Gold: "env-" + x, Answer: t, Present: true}
		}
	}

	items := make([]item, 0, n)
	present := make([]probe, 0, n)
	for i := 0; i < n; i++ {
		it, pr := mk(i)
		items = append(items, it)
		present = append(present, pr)
	}
	// Absent probes: questions about coined subjects past the seeded range.
	absent := make([]probe, 0, nAbsent)
	for i := 0; i < nAbsent; i++ {
		_, pr := mk(n + 1 + i)
		pr.Gold = ""
		pr.Answer = ""
		pr.Present = false
		absent = append(absent, pr)
	}
	return items, present, absent
}
