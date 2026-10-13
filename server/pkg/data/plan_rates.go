package data

import "time"

type rateQueryKey struct {
	policy  string
	subject string
}

type rateQuery struct {
	at     time.Time
	events []RateEvent
}

type ratePlan struct {
	queries map[rateQueryKey]rateQuery
	pending []RateEvent
}

func (p *mutationPlan) rateState() *ratePlan {
	if p.rates == nil {
		p.rates = &ratePlan{queries: make(map[rateQueryKey]rateQuery)}
	}
	return p.rates
}
