package main

// genFacts builds a valid retrieval/gate test instrument:
//   - present memories use globally-unique REAL-word subjects with genuinely
//     distinct facts (no shared template, no coined-sibling subword collisions),
//     so a query naming a subject has exactly one strong match.
//   - present probes paraphrase the fact (different words from the stored
//     content) so retrieval tests semantics, not lexical overlap.
//   - absent probes ask about topics that appear in NO seeded memory at all
//     (entirely unrelated domains), so a correct gate must suppress them. This
//     avoids the "topic present, exact entity absent" contamination that made
//     the synthetic corpora unable to measure suppression.
func genFacts() ([]item, []probe, []probe) {
	type fact struct{ concept, content, question string }
	facts := []fact{
		{"capybara", "The capybara is the largest living rodent and is highly social, often seen resting beside birds and small monkeys.", "Which animal is the biggest rodent alive today?"},
		{"axolotl", "The axolotl is a salamander that stays aquatic for life and can regenerate entire lost limbs and parts of its heart.", "Which amphibian can regrow its own limbs and heart tissue?"},
		{"pangolin", "The pangolin is the only mammal fully covered in keratin scales and rolls into a ball when threatened.", "What mammal is covered in scales and curls into a ball?"},
		{"narwhal", "The narwhal is an Arctic whale whose males grow a single long spiral tusk that is actually an enlarged tooth.", "Which whale has a long spiral tusk made from a tooth?"},
		{"tardigrade", "The tardigrade can survive the vacuum of space and extreme dehydration by entering a cryptobiotic tun state.", "Which tiny creature can survive in outer space?"},
		{"okapi", "The okapi looks like a cross between a zebra and a horse but is the giraffe's closest living relative.", "What striped-legged animal is the giraffe's closest relative?"},

		{"saffron", "Saffron is the world's most expensive spice by weight because each crocus flower yields only three hand-picked stigmas.", "Why is saffron so costly per gram?"},
		{"kimchi", "Kimchi is a Korean dish of vegetables fermented with chili, garlic, and ginger in brine.", "What is the fermented Korean vegetable dish made with chili and garlic?"},
		{"miso", "Miso is a Japanese paste made by fermenting soybeans with salt and the koji mold.", "What Japanese seasoning comes from fermenting soybeans with koji?"},
		{"tahini", "Tahini is a paste ground from toasted sesame seeds and is a base for hummus and halva.", "What sesame-seed paste is used to make hummus?"},
		{"harissa", "Harissa is a North African hot paste built from roasted red chilies, garlic, caraway, and coriander.", "Which North African chili paste contains caraway and coriander?"},

		{"helium", "Helium is the second lightest element and the only one that cannot be solidified at normal atmospheric pressure.", "Which element will not freeze solid under ordinary pressure?"},
		{"mercury", "Mercury is the only metal that is liquid at room temperature and was once used in thermometers.", "Which metal stays liquid at room temperature?"},
		{"tungsten", "Tungsten has the highest melting point of any pure metal and is used for light-bulb filaments.", "Which metal has the highest melting point?"},
		{"bismuth", "Bismuth forms iridescent rainbow crystals with a distinctive stair-stepped hopper shape.", "Which element grows colorful stair-stepped rainbow crystals?"},
		{"neon", "Neon glows a bright reddish-orange when an electric current passes through it in a sealed tube.", "Which gas glows orange-red in an electrified tube?"},

		{"theremin", "The theremin is played without any physical contact by moving the hands near two antennas.", "Which instrument is played without touching it?"},
		{"sitar", "The sitar is a long-necked Indian string instrument with sympathetic strings that resonate on their own.", "Which Indian string instrument has resonating sympathetic strings?"},
		{"bagpipes", "The bagpipes produce continuous sound because air stored in a bag feeds the reeds while the player breathes.", "Which instrument plays continuously using air stored in a bag?"},
		{"didgeridoo", "The didgeridoo is an Aboriginal Australian wind instrument played with circular breathing for a constant drone.", "Which Australian wind instrument uses circular breathing?"},

		{"curling", "Curling is played by sliding granite stones toward a target while teammates sweep the ice to steer them.", "Which sport involves sweeping the ice to guide sliding stones?"},
		{"sumo", "Sumo is a Japanese wrestling sport where you win by forcing your opponent out of the ring or to touch the ground.", "In which sport do you push your opponent out of a ring to win?"},
		{"capoeira", "Capoeira is a Brazilian art blending martial arts, dance, and acrobatics performed to live music.", "Which Brazilian discipline mixes martial arts with dance and music?"},
		{"sepak", "Sepak takraw is a Southeast Asian sport like volleyball but played by kicking a rattan ball over a net.", "Which sport is volleyball played with the feet and a rattan ball?"},

		{"Reykjavik", "Reykjavik is the world's northernmost capital and is heated almost entirely by geothermal energy.", "Which capital city runs mostly on geothermal heating?"},
		{"Venice", "Venice is built across 118 small islands linked by canals and has no roads for cars in its center.", "Which city is built on islands and has canals instead of streets?"},
		{"Petra", "Petra is an ancient city in Jordan carved directly into rose-colored sandstone cliffs.", "Which ancient city was carved into pink sandstone cliffs?"},
		{"Timbuktu", "Timbuktu was a medieval center of Islamic scholarship on the edge of the Sahara with vast manuscript libraries.", "Which Saharan city was a famous medieval center of learning?"},

		{"photosynthesis", "Photosynthesis converts sunlight, water, and carbon dioxide into sugar and releases oxygen as a byproduct.", "How do plants turn sunlight and CO2 into food?"},
		{"mycelium", "Mycelium is the underground thread network of a fungus and can link trees to share nutrients.", "What underground fungal network connects trees to share nutrients?"},
		{"tannin", "Tannins are bitter plant compounds that give red wine and strong tea their dry, puckering mouthfeel.", "What compound makes red wine and tea taste dry and astringent?"},
		{"chlorophyll", "Chlorophyll is the green pigment that absorbs red and blue light and reflects green.", "Which pigment makes leaves green by reflecting green light?"},
	}

	items := make([]item, 0, len(facts))
	present := make([]probe, 0, len(facts))
	for _, f := range facts {
		items = append(items, item{Concept: f.concept, Content: f.content})
		present = append(present, probe{Query: f.question, Gold: f.concept, Present: true})
	}

	// Absent probes: questions about domains with zero seeded memories.
	absentQs := []string{
		"What is the recommended tire pressure for a loaded cargo van?",
		"How do I file a quarterly estimated tax payment as a freelancer?",
		"What stitch should I use to bind off a knitted scarf?",
		"How often should I replace the brake fluid in my car?",
		"What is the proper way to floss around a dental bridge?",
		"Which chess opening best counters the Sicilian Defense?",
		"How long should I proof sourdough before baking in winter?",
		"What is a good beginner route grade for outdoor rock climbing?",
		"How do I deadhead petunias to keep them flowering?",
		"What shutter speed freezes a hummingbird's wings in a photo?",
		"How do I reconcile a bank statement with double-entry bookkeeping?",
		"What is the correct torque for lug nuts on an alloy wheel?",
		"How do I remove a red wine stain from a wool carpet?",
		"What is the offside rule in association football?",
		"How do I set up a drip irrigation timer for a vegetable bed?",
		"What is the difference between a mortgage APR and the interest rate?",
		"How do I tune a drop-D guitar from standard tuning?",
		"What is a safe internal temperature for roast chicken?",
		"How do I clear a paper jam in a laser printer?",
		"What yoga pose helps relieve lower back tension?",
		"How do I calculate the area of a triangle from three side lengths?",
		"What is the best bait for catching freshwater bass in spring?",
		"How do I whiten grout between bathroom tiles?",
		"What is the recommended dose of fertilizer for tomato seedlings?",
	}
	absent := make([]probe, 0, len(absentQs))
	for _, q := range absentQs {
		absent = append(absent, probe{Query: q, Gold: "", Present: false})
	}
	return items, present, absent
}
