package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"adesgo/internal/llm"
)

// TitleJob gives LLM-generated titles to campaigns that have none (titles
// renamed by hand are never touched). It runs in the background, paced for
// the free tier's per-minute limit, and stops at the daily quota: untitled
// campaigns are simply picked up by the next run.
type TitleJob struct {
	svc *Service
	llm *llm.Client

	mu      sync.Mutex
	running bool
	status  TitleStatus
}

type TitleStatus struct {
	Running bool
	Done    int // titles saved in the current/last run
	Total   int // untitled campaigns at the start of the run
	Message string
}

// pace keeps us under OpenRouter's free limit of 20 requests per minute.
const titlePace = 3500 * time.Millisecond

func NewTitleJob(svc *Service, client *llm.Client) *TitleJob {
	return &TitleJob{svc: svc, llm: client}
}

func (j *TitleJob) Status() TitleStatus {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.status
}

// Start launches a run unless one is already going. Returns false if busy.
func (j *TitleJob) Start(ctx context.Context) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.running {
		return false
	}
	j.running = true
	j.status = TitleStatus{Running: true, Message: "Avvio…"}
	go j.run(context.WithoutCancel(ctx))
	return true
}

func (j *TitleJob) set(f func(*TitleStatus)) {
	j.mu.Lock()
	f(&j.status)
	j.mu.Unlock()
}

func (j *TitleJob) run(ctx context.Context) {
	defer func() {
		j.mu.Lock()
		j.running = false
		j.status.Running = false
		j.mu.Unlock()
	}()
	if !j.llm.Enabled() {
		j.set(func(s *TitleStatus) { s.Message = "OPENROUTER_API_KEY non configurata." })
		return
	}
	rows, err := j.svc.ListingRows(ctx)
	if err != nil {
		j.set(func(s *TitleStatus) { s.Message = "Errore: " + err.Error() })
		return
	}
	var todo []Campaign
	for _, c := range GroupCampaigns(rows) { // most recently active first
		if c.Title == "" {
			todo = append(todo, c)
		}
	}
	j.set(func(s *TitleStatus) { s.Total = len(todo); s.Message = "" })
	if len(todo) == 0 {
		j.set(func(s *TitleStatus) { s.Message = "Tutte le inserzioni hanno già un titolo." })
		return
	}

	failed := 0
	for i, c := range todo {
		if i > 0 {
			time.Sleep(titlePace)
		}
		title, err := j.llm.Title(ctx, c.Testo)
		if errors.Is(err, llm.ErrDailyCap) {
			j.set(func(s *TitleStatus) {
				s.Message = fmt.Sprintf("Quota giornaliera OpenRouter esaurita: %d titoli salvati, i restanti %d verranno elaborati dopo il reset.",
					s.Done, len(todo)-i)
			})
			log.Printf("[titles] daily quota reached after %d titles", j.Status().Done)
			return
		}
		if err != nil {
			failed++
			log.Printf("[titles] %q: %v", c.Key, err)
			continue
		}
		if err := j.svc.RenameCampaign(ctx, c.MsgIDs, title); err != nil {
			log.Printf("[titles] save %q: %v", c.Key, err)
			continue
		}
		j.set(func(s *TitleStatus) { s.Done++ })
	}
	j.set(func(s *TitleStatus) {
		s.Message = fmt.Sprintf("Completato: %d titoli salvati", s.Done)
		if failed > 0 {
			s.Message += fmt.Sprintf(", %d non riusciti (riprovati al prossimo giro)", failed)
		}
		s.Message += "."
	})
	log.Printf("[titles] run done: %d saved, %d failed", j.Status().Done, failed)
}

// Loop starts a run every interval, so new listings get a title too.
func (j *TitleJob) Loop(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if j.llm.Enabled() {
				j.Start(ctx)
			}
		}
	}
}
