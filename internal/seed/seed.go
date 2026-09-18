// Package seed carries the profiles this board ships with: the people it
// hunts for, described well enough that a prep or apply stage can read the
// board instead of a CV file kept somewhere else.
//
// Seeding never overwrites. Each profile is written field by field into the
// empty fields of the row with that slug, so a description edited through the
// API survives every later deploy, and a field added to a seed later still
// lands. The text below is transcribed from the CVs and the master profile —
// nothing here is invented, and a fact the source does not state is left out
// rather than guessed at.
package seed

import (
	"time"

	"github.com/sergey-yakushevich/jobhub/internal/store"
)

// Apply writes every seed profile and reports the slugs that gained anything.
// It runs on boot: a fresh database comes up with the profiles described, and
// an existing one gains only what it was missing.
func Apply(st *store.Store, now time.Time) ([]string, error) {
	var written []string
	for _, p := range Profiles {
		ok, err := st.SeedProfile(p, now)
		if err != nil {
			return written, err
		}
		if ok {
			written = append(written, p.Slug)
		}
	}
	return written, nil
}

// Profiles is the shipped set. Slugs match the profile names the sweeps
// already tag leads with, which is what ties a description to a board.
var Profiles = []store.ProfileParams{sergey, polina, siarhei}

// sergey is transcribed from jobhunt/profiles/sergey.yaml — the master profile
// every tailored CV is built from. The conditions section holds what that file
// actually states: where he is, which location each kind of posting gets, and
// his English. Rate and notice are not in it, so they are not here either.
var sergey = store.ProfileParams{
	Slug:     "sergey",
	Name:     "Sergey Yakushevich",
	Headline: "Senior Backend Engineer — Go (Golang), Ruby",
	Summary: `Senior Backend Engineer (Go, Ruby). 10 years on payment backends, shipping Golang in production since 2024. Kafka, PostgreSQL, AWS, PCI-DSS.

I have spent 10 years on backends, and the last 5 of them on payment systems. At Moyasar I built fraud blocking and regulatory KYC on a platform that has processed 350M+ payments across 10+ microservices. Before that I shipped Visa installments and PCI-DSS card tokenization at Mondido, where I also moved our first services to Go, and built banking integrations at Regate. I take a feature from schema design through to the alert that pages someone when it breaks in production. I have written Go in production since 2024 and Ruby for most of the decade before it, and I am looking for backend roles in Go or Ruby.`,
	Skills: `Languages: Go (Golang), Ruby, Ruby on Rails
Data: PostgreSQL, Redis, Elasticsearch, Kafka
Infrastructure: Docker, Kubernetes, Terraform, AWS (EC2, RDS, S3, Lambda, ECS), CI/CD (GitHub Actions)
Architecture: Concurrency & message queues, event-driven systems, microservices, distributed systems, API design (REST & GraphQL), OAuth2 / JWT
Payments: Payment gateway integration, PCI-DSS compliance, 3DS authentication, card tokenization, fraud detection, identity verification (KYC)
Other: English (C1)`,
	Experience: `Moyasar — Senior Engineer, Dec 2025 – Aug 2026 (Ruby on Rails, PostgreSQL, Microservices, Kafka, Fraud Detection)
• Built the fraud system that scores and blocks transactions on risk signals, on a platform that has taken 350M+ payments.
• Shipped identity verification to the regulator's spec, which we needed to expand into new markets.
• Wrote the alerting for payment flow disruptions and took time to detection from hours to seconds.
• Analysed on his own initiative which platform features merchants actually use, and handed management the numbers.
• Sat in on sales calls as the technical voice when a prospective merchant had integration questions.

Mondido — Senior Engineer, Jan 2024 – Nov 2025 (Go, Ruby on Rails, React, PostgreSQL, PCI-DSS, Card Tokenization)
• Wrote Go services on the platform, taking the high-throughput parts of the gateway integration off Rails.
• Delivered the Visa installment integration, backend and checkout UI.
• Built PCI-DSS compliant card tokenization, so raw card numbers stayed out of our systems.
• Built the audit log that traces every merchant and staff action without adding latency to the request path.

Regate — Senior Engineer, Nov 2021 – Jan 2024 (Kafka, Event-Driven, AWS, Kubernetes, Terraform, Ruby on Rails)
• Owned the integrations with the payment and banking APIs that drive automated accounting workflows.
• Started the service-layer refactor that pulled business logic out of callback-heavy Rails models.
• Raised RSpec coverage and added CloudWatch dashboards, so regressions surfaced in CI rather than in production.

iTransition — Software Engineer, Jan 2016 – Jun 2020 (PostgreSQL, Kafka, Elasticsearch, Microservices, AWS ECS, Ruby on Rails)
• Moved the main search workload off MySQL onto Elasticsearch and made it 90% faster for 50M+ users.
• Made the test pipeline 5x faster, taking a full release cycle from days to hours.
• Tuned the PostgreSQL layer: replaced stale indexes, rewrote the slowest reads, cut reporting load on the primary.

Education: Belarusian State University, BSc Computer Science, 2013 – 2018.`,
	Conditions: `Two CVs, identical except the location line:
• Batumi, Georgia — the truth. Used for worldwide, global, EMEA and unspecified-remote postings.
• Warsaw, Poland — used for EU-restricted roles ("remote anywhere in the EU", per-country EU entities).
English: C1.
Source: jobhunt/profiles/sergey.yaml. It states no rate or notice period, so neither is recorded here.`,
	Links: `https://cyberjosef.dev
https://github.com/sergey-yakushevich
https://linkedin.com/in/sergey-yakushevich-688a4b179/
sergeyayya@gmail.com`,
}

// polina is transcribed from "Polina Avdevich — AI Creative Producer.pdf".
var polina = store.ProfileParams{
	Slug:     "polina",
	Name:     "Polina Avdevich",
	Headline: "AI Creative Producer (UGC, Video Ads & Social Content)",
	Summary: `AI Creative Producer — AI-generated UGC, AI videos and ad creatives for brands. Started in art school: seven years of drawing, painting and composition, then three more at a college of arts — interior design, typography, logos and brand identity — before working as a photographer, in-house designer for a large group of shopping malls, and visual merchandiser.

In 2023, starting her own streetwear brand, she found nobody who would make the ads she saw in her head, and the sales depended on them — so she learned generative tools herself, on the earliest models, by trial and error. Word of mouth turned that into paid work: restaurant menus and campaigns, AI-designed tarot decks, anything that built the hand. Then she built her own product brand, where she did everything herself — from the product itself to the website and the ad creatives. Now she does that for other brands.`,
	Skills: `Generative AI, art direction, video production, video editing, advertising, ads management
Social media management, content creation, branding & identity, logo design, graphic design
Photography, e-commerce
Tools: Midjourney, Nano Banana, Gemini, Higgsfield, Veo, Sora, Kling, Seedance, ElevenLabs, CapCut, Canva, Claude Code`,
	Experience: `SickStuff — Founder & Creative Director, Dec 2025 – present (Art Direction, Generative AI, Branding & Identity, E-Commerce)
• Founded a tech-accessories brand for the Apple ecosystem: product, manufacturing, brand and store built solo, from empty file to a shop that ships.
• Developed the line and a proprietary production technique of her own design — the brand's main point of difference.
• Built the website, shot the product photography, and produces every ad creative and AI video campaign for the brand.

Self-employed — AI Creative Producer, Jan 2023 – present (Generative AI, Video Production, Advertising, Social Media, Video Editing)
• Produces AI-generated UGC, product videos and ad creatives full cycle — concept, generation, edit, delivery, plus the landing pages.
• Builds UGC-style ads on consistent AI characters: a full campaign on one face, no casting and no shoot day.
• Delivers photorealistic product imagery and video for brands with no studio budget, replacing whole production days.
• Runs the channel too — content plan, social media management, posting and scheduling, ad account setup and creative testing.

Vermin — Founder, Jul 2024 – Nov 2024 (Fashion Design, Apparel, E-Commerce)
• Founded a streetwear label and took it from first sketch to a sold-out run.
• Designed the graphics and the full product line, and managed the limited production run end to end.

Kvibis LLC — Graphic Designer, Jul 2023 – Jul 2024 (Graphic Design, Branding & Identity, Logo Design, Wide Format Printing)
• Sole in-house designer for a large group of shopping and entertainment centres in Minsk.
• Designed logos, identities and business cards alongside print and digital campaigns.

TSUM Minsk — Visual Merchandiser, Window Display Designer, Jan 2022 – Jun 2022
• Designed, hand-painted and hand-built in-store displays and seasonal decor for a flagship department store.

Photography Studio, Minsk — Photographer, Jan 2020 – Jan 2022
• Shot portrait, children's, family and pet sessions in studio and on location, and handled all post-production.

Education: Minsk State College of Arts, Professional Diploma in Design — Interior Design and Applied Arts, 2020 – 2023. Minsk Art School No. 2, Fine and Studio Arts, 2013 – 2020.
Certificate: Certified Folk Master — Applied Arts, Ministry of Culture of Belarus, 2023.`,
	Conditions: `Based in Batumi, Georgia. Remote.
Open to freelance and full-time roles with brands in the EU and US.
Source: "Polina Avdevich — AI Creative Producer.pdf".`,
	Links: `https://sy4iz.com
https://linkedin.com/in/polina-avdevich-049a06428
olgaavdevich80@gmail.com`,
}

// siarhei is the same career as sergey under the Belarusian spelling of his
// name and a Minsk location — the CV at Resumes/Sergey_2. It is a separate
// profile rather than a note on his, because the board files leads by the
// identity they were sent under, and these two are sent under different ones.
var siarhei = store.ProfileParams{
	Slug:     "siarhei",
	Name:     "Siarhei Lyagushevich",
	Headline: "Senior Backend Engineer — Go (Golang), Ruby",
	Summary: `Senior Backend Engineer (Go, Ruby), based in Minsk, Belarus. The same career as the sergey profile, applied for under the Belarusian spelling of his name and a Minsk location line.

I have spent 10 years on backends, and the last 5 of them on payment systems. At Moyasar I built fraud blocking and regulatory KYC on a platform that has processed 350M+ payments across 10+ microservices. Before that I shipped Visa installments and PCI-DSS card tokenization at Mondido, where I also moved our first services to Go and built banking integrations at Regate. I take a feature from schema design through to the alert that pages someone when it breaks in production. I have written Go in production since 2024 and Ruby for most of the decade before it, and I am looking for backend roles in Go or Ruby.`,
	Skills: `Go (Golang), Ruby, Ruby on Rails
Concurrency & message queues, Kafka, event-driven systems, microservices, distributed systems
API design (REST & GraphQL), OAuth2 / JWT
PostgreSQL, Redis, Elasticsearch
Docker, Kubernetes, Terraform, AWS (EC2, RDS, S3, Lambda, ECS), CI/CD (GitHub Actions)
Payment gateway integration, PCI-DSS compliance, 3DS authentication, card tokenization, fraud detection, identity verification (KYC)
English (C1)`,
	Experience: `Moyasar — Senior Engineer, Dec 2025 – Aug 2026 (Ruby on Rails, PostgreSQL, Microservices, Kafka, Fraud Detection)
• Built the fraud system that scores and blocks transactions on risk signals, on a platform that has taken 350M+ payments.
• Shipped identity verification to the regulator's spec, needed to expand into new markets.
• Wrote the alerting for payment flow disruptions and took time to detection from hours to seconds.
• Analysed on his own initiative which platform features merchants actually use, and handed management the numbers. Sales now leads with the most adopted features when pitching new clients.
• Sat in on sales calls as the technical voice when a prospective merchant had integration questions.

Mondido — Senior Engineer, Jan 2024 – Nov 2025 (Go, Ruby on Rails, React, PostgreSQL, PCI-DSS, Card Tokenization)
• Wrote Go services on the platform, taking the high-throughput parts of the gateway integration off Rails and onto a runtime that handles concurrent calls without a worker pool per request.
• Delivered the Visa installment integration, backend and checkout UI.
• Built PCI-DSS compliant card tokenization for gateway calls, so raw card numbers stayed out of our systems.
• Built the audit log that traces every merchant and staff action without adding latency to the request path.

Regate — Senior Engineer, Nov 2021 – Jan 2024 (Kafka, Event-Driven, AWS, Kubernetes, Terraform, Ruby on Rails)
• Owned the integrations with the payment and banking APIs that drive the automated accounting workflows.
• Started the service-layer refactor that pulled business logic out of callback-heavy Rails models.
• Raised RSpec coverage and added CloudWatch dashboards, so regressions surfaced in CI rather than in production.

iTransition — Software Engineer, Jan 2018 – Jun 2020 (PostgreSQL, Kafka, Elasticsearch, Microservices, AWS ECS, Ruby on Rails)
• Moved the main search workload off MySQL onto Elasticsearch and made it 90% faster for 50M+ users.
• Made the test pipeline 5x faster, which took a full release cycle down from days to hours.
• Tuned the PostgreSQL layer under the platform: replaced indexes that no longer matched the query patterns, rewrote the slowest reads, and cut the load the reporting queries put on the primary.

Education: Belarusian State University, BSc Computer Science, 2013 – 2018.`,
	Conditions: `Location line on this CV: Minsk, Belarus. Remote.
English: C1.
Source: Resumes/Sergey_2/Siarhei_Lyagushevich_—_Senior_Backend_Engineer_—_Go_Golang,_Ruby.pdf. It states no rate or notice period, so neither is recorded here.`,
	Links: `https://cyberjosef.dev
https://github.com/sergey-yakushevich
https://linkedin.com/in/sergey-yakushevich-688a4b179
sergeyayya@gmail.com
+48530213401`,
}
