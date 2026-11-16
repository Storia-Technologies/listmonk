package main

// Sequences (drip campaigns).
//
// This file is a Storia addition to listmonk and is deliberately self-contained
// so that upstream releases merge cleanly: its tables are created here (not in
// schema.sql or the versioned migrations), its SQL is inline (not in queries/),
// and upstream files only carry one-line hooks into it.
//
// Model: a sequence watches one list. Anyone who joins that list after the
// sequence is switched on is enrolled, and is then sent each step in order,
// each one a set wait after the one before it. A step's e-mail is a normal
// draft campaign, so it is written in the normal campaign editor and its opens,
// clicks, unsubscribe link and bounces all behave like any other campaign.

import (
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
	null "gopkg.in/volatiletech/null.v6"
)

const (
	seqStatusActive = "active"
	seqStatusPaused = "paused"

	seqEnrolActive    = "active"
	seqEnrolCompleted = "completed"
	seqEnrolExited    = "exited"

	// How often the engine looks for people to enrol and steps that are due.
	seqTickInterval = time.Minute

	// The most step e-mails queued per sequence in one tick. Anything beyond
	// this is picked up on the next tick.
	seqBatchSize = 500

	// The longest wait allowed before a step (one year).
	seqMaxDelayMinutes = 525600
)

const seqSchema = `
CREATE TABLE IF NOT EXISTS sequences (
    id              SERIAL PRIMARY KEY,
    name            TEXT NOT NULL,
    list_id         INTEGER NOT NULL,
    exit_list_ids   INTEGER[] NOT NULL DEFAULT '{}',
    status          TEXT NOT NULL DEFAULT 'paused',
    activated_at    TIMESTAMP WITH TIME ZONE NULL,
    last_error      TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at      TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    CONSTRAINT list_is_used_by_a_sequence FOREIGN KEY (list_id) REFERENCES lists(id) ON DELETE RESTRICT
);

CREATE TABLE IF NOT EXISTS sequence_steps (
    id              SERIAL PRIMARY KEY,
    sequence_id     INTEGER NOT NULL REFERENCES sequences(id) ON DELETE CASCADE,
    campaign_id     INTEGER NOT NULL UNIQUE,
    position        INTEGER NOT NULL DEFAULT 0,
    delay_minutes   INTEGER NOT NULL DEFAULT 0,
    created_at      TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at      TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    CONSTRAINT campaign_is_used_by_a_sequence FOREIGN KEY (campaign_id) REFERENCES campaigns(id) ON DELETE RESTRICT
);
CREATE INDEX IF NOT EXISTS idx_sequence_steps_seq ON sequence_steps(sequence_id, position);

CREATE TABLE IF NOT EXISTS sequence_enrolments (
    id              BIGSERIAL PRIMARY KEY,
    sequence_id     INTEGER NOT NULL REFERENCES sequences(id) ON DELETE CASCADE,
    subscriber_id   INTEGER NOT NULL REFERENCES subscribers(id) ON DELETE CASCADE,
    status          TEXT NOT NULL DEFAULT 'active',
    exit_reason     TEXT NOT NULL DEFAULT '',
    last_position   INTEGER NOT NULL DEFAULT 0,
    last_sent_at    TIMESTAMP WITH TIME ZONE NULL,
    enrolled_at     TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at      TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    UNIQUE (sequence_id, subscriber_id)
);
CREATE INDEX IF NOT EXISTS idx_sequence_enrolments_due ON sequence_enrolments(sequence_id, status);

CREATE TABLE IF NOT EXISTS sequence_sends (
    id              BIGSERIAL PRIMARY KEY,
    sequence_id     INTEGER NOT NULL REFERENCES sequences(id) ON DELETE CASCADE,
    step_id         INTEGER NOT NULL REFERENCES sequence_steps(id) ON DELETE CASCADE,
    subscriber_id   INTEGER NOT NULL REFERENCES subscribers(id) ON DELETE CASCADE,
    campaign_id     INTEGER NOT NULL,
    created_at      TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    UNIQUE (step_id, subscriber_id)
);
CREATE INDEX IF NOT EXISTS idx_sequence_sends_seq ON sequence_sends(sequence_id);
`

type sequence struct {
	ID          int           `db:"id" json:"id"`
	Name        string        `db:"name" json:"name"`
	ListID      int           `db:"list_id" json:"list_id"`
	ListName    string        `db:"list_name" json:"list_name"`
	ExitListIDs pq.Int64Array `db:"exit_list_ids" json:"exit_list_ids"`
	Status      string        `db:"status" json:"status"`
	ActivatedAt null.Time     `db:"activated_at" json:"activated_at"`
	LastError   string        `db:"last_error" json:"last_error"`
	CreatedAt   null.Time     `db:"created_at" json:"created_at"`
	UpdatedAt   null.Time     `db:"updated_at" json:"updated_at"`

	NumSteps     int `db:"num_steps" json:"num_steps"`
	NumActive    int `db:"num_active" json:"num_active"`
	NumCompleted int `db:"num_completed" json:"num_completed"`
	NumExited    int `db:"num_exited" json:"num_exited"`
	NumSent      int `db:"num_sent" json:"num_sent"`

	Steps []sequenceStep `db:"-" json:"steps,omitempty"`
}

type sequenceStep struct {
	ID              int    `db:"id" json:"id"`
	SequenceID      int    `db:"sequence_id" json:"sequence_id"`
	CampaignID      int    `db:"campaign_id" json:"campaign_id"`
	CampaignName    string `db:"campaign_name" json:"campaign_name"`
	CampaignSubject string `db:"campaign_subject" json:"campaign_subject"`
	Position        int    `db:"position" json:"position"`
	DelayMinutes    int    `db:"delay_minutes" json:"delay_minutes"`
	NumSent         int    `db:"num_sent" json:"num_sent"`
	NumViews        int    `db:"num_views" json:"num_views"`
	NumClicks       int    `db:"num_clicks" json:"num_clicks"`
}

type sequenceEnrolment struct {
	ID           int64     `db:"id" json:"id"`
	SubscriberID int       `db:"subscriber_id" json:"subscriber_id"`
	Email        string    `db:"email" json:"email"`
	Name         string    `db:"name" json:"name"`
	Status       string    `db:"status" json:"status"`
	ExitReason   string    `db:"exit_reason" json:"exit_reason"`
	LastPosition int       `db:"last_position" json:"last_position"`
	LastSentAt   null.Time `db:"last_sent_at" json:"last_sent_at"`
	EnrolledAt   null.Time `db:"enrolled_at" json:"enrolled_at"`
	NextDueAt    null.Time `db:"next_due_at" json:"next_due_at"`
	Total        int       `db:"total" json:"-"`
}

type sequenceReq struct {
	Name        string `json:"name"`
	ListID      int    `json:"list_id"`
	ExitListIDs []int  `json:"exit_list_ids"`
}

type sequenceStepReq struct {
	CampaignID   int `json:"campaign_id"`
	DelayMinutes int `json:"delay_minutes"`
}

const seqSelect = `
SELECT s.id, s.name, s.list_id, COALESCE(l.name, '') AS list_name, s.exit_list_ids, s.status,
    s.activated_at, s.last_error, s.created_at, s.updated_at,
    (SELECT COUNT(*) FROM sequence_steps st WHERE st.sequence_id = s.id) AS num_steps,
    (SELECT COUNT(*) FROM sequence_enrolments e WHERE e.sequence_id = s.id AND e.status = 'active') AS num_active,
    (SELECT COUNT(*) FROM sequence_enrolments e WHERE e.sequence_id = s.id AND e.status = 'completed') AS num_completed,
    (SELECT COUNT(*) FROM sequence_enrolments e WHERE e.sequence_id = s.id AND e.status = 'exited') AS num_exited,
    (SELECT COUNT(*) FROM sequence_sends sn WHERE sn.sequence_id = s.id) AS num_sent
FROM sequences s LEFT JOIN lists l ON l.id = s.list_id
`

// sequencesEnabled reports whether the feature is switched on for this
// instance. It is on unless LISTMONK_sequences__enabled is set to false.
func sequencesEnabled() bool {
	if !ko.Exists("sequences.enabled") {
		return true
	}
	return ko.Bool("sequences.enabled")
}

// initSequences creates the sequence tables if they don't exist and starts
// the engine. It is called once on boot.
func (a *App) initSequences() {
	if !sequencesEnabled() {
		a.log.Println("sequences are disabled on this instance")
		return
	}

	if _, err := a.db.Exec(seqSchema); err != nil {
		a.log.Printf("error creating sequence tables, sequences will not run: %v", err)
		return
	}

	go func() {
		t := time.NewTicker(seqTickInterval)
		defer t.Stop()
		for range t.C {
			a.runSequences()
		}
	}()
}

// initSequenceHandlers registers the sequence API on the authenticated group.
func initSequenceHandlers(g *echo.Group, a *App) {
	if !sequencesEnabled() {
		return
	}

	var (
		pm     = a.auth.Perm
		get    = []string{"campaigns:get_all", "campaigns:get"}
		manage = []string{"campaigns:manage_all", "campaigns:manage"}
	)

	g.GET("/api/sequences", pm(a.GetSequences, get...))
	g.GET("/api/sequences/:id", pm(hasID(a.GetSequence), get...))
	g.GET("/api/sequences/:id/subscribers", pm(hasID(a.GetSequenceSubscribers), get...))
	g.POST("/api/sequences", pm(a.CreateSequence, manage...))
	g.PUT("/api/sequences/:id", pm(hasID(a.UpdateSequence), manage...))
	g.PUT("/api/sequences/:id/status", pm(hasID(a.UpdateSequenceStatus), "campaigns:send"))
	g.DELETE("/api/sequences/:id", pm(hasID(a.DeleteSequence), manage...))
	g.POST("/api/sequences/:id/steps", pm(hasID(a.CreateSequenceStep), manage...))
	g.PUT("/api/sequences/:id/steps/order", pm(hasID(a.OrderSequenceSteps), manage...))
	g.PUT("/api/sequences/:id/steps/:stepID", pm(hasID(a.UpdateSequenceStep), manage...))
	g.DELETE("/api/sequences/:id/steps/:stepID", pm(hasID(a.DeleteSequenceStep), manage...))
	g.DELETE("/api/sequences/:id/subscribers/:subID", pm(hasID(a.RemoveSequenceSubscriber), manage...))
}

// checkSequenceCampaign stops a campaign that is a sequence step from being
// started or scheduled as a normal campaign, which would send it to the whole
// list at once.
func (a *App) checkSequenceCampaign(campID int, status string) error {
	if !sequencesEnabled() {
		return nil
	}
	if status != models.CampaignStatusRunning && status != models.CampaignStatusScheduled {
		return nil
	}

	var name string
	err := a.db.Get(&name, `SELECT s.name FROM sequence_steps st
		JOIN sequences s ON s.id = st.sequence_id WHERE st.campaign_id = $1`, campID)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		// The tables may not exist yet on a fresh boot. Don't block campaigns on that.
		return nil
	}

	return echo.NewHTTPError(http.StatusBadRequest,
		fmt.Sprintf("This e-mail is a step in the sequence \"%s\" and is sent by it automatically. "+
			"Remove it from the sequence first to send it as a normal campaign.", name))
}

// GetSequences returns all sequences with their counts.
func (a *App) GetSequences(c echo.Context) error {
	out := []sequence{}
	if err := a.db.Select(&out, seqSelect+` ORDER BY s.created_at DESC`); err != nil {
		return seqDBErr(a, "fetching sequences", err)
	}

	return c.JSON(http.StatusOK, okResp{out})
}

// GetSequence returns one sequence with its steps.
func (a *App) GetSequence(c echo.Context) error {
	out, err := a.getSequence(getID(c))
	if err != nil {
		return err
	}

	return c.JSON(http.StatusOK, okResp{out})
}

// GetSequenceSubscribers returns the people enrolled in a sequence.
func (a *App) GetSequenceSubscribers(c echo.Context) error {
	var (
		id     = getID(c)
		status = c.QueryParam("status")
		pg     = a.pg.NewFromURL(c.Request().URL.Query())
	)

	res := []sequenceEnrolment{}
	if err := a.db.Select(&res, `
		SELECT e.id, e.subscriber_id, sub.email, sub.name, e.status, e.exit_reason, e.last_position,
			e.last_sent_at, e.enrolled_at,
			(CASE WHEN e.status = 'active' THEN
				COALESCE(e.last_sent_at, e.enrolled_at) + (nxt.delay_minutes * INTERVAL '1 minute')
			END) AS next_due_at,
			COUNT(*) OVER () AS total
		FROM sequence_enrolments e
		JOIN subscribers sub ON sub.id = e.subscriber_id
		LEFT JOIN LATERAL (
			SELECT delay_minutes FROM sequence_steps st
			WHERE st.sequence_id = e.sequence_id AND st.position > e.last_position
			ORDER BY st.position LIMIT 1
		) nxt ON TRUE
		WHERE e.sequence_id = $1 AND ($2 = '' OR e.status = $2)
		ORDER BY e.enrolled_at DESC, e.id DESC OFFSET $3 LIMIT (CASE WHEN $4 < 1 THEN NULL ELSE $4 END)`,
		id, status, pg.Offset, pg.Limit); err != nil {
		return seqDBErr(a, "fetching sequence subscribers", err)
	}

	total := 0
	if len(res) > 0 {
		total = res[0].Total
	}

	return c.JSON(http.StatusOK, okResp{models.PageResults{
		Results: res,
		Total:   total,
		Page:    pg.Page,
		PerPage: pg.PerPage,
	}})
}

// CreateSequence creates a new, paused sequence.
func (a *App) CreateSequence(c echo.Context) error {
	var req sequenceReq
	if err := c.Bind(&req); err != nil {
		return err
	}
	if err := a.validateSequenceReq(&req); err != nil {
		return err
	}

	var id int
	if err := a.db.Get(&id, `INSERT INTO sequences (name, list_id, exit_list_ids) VALUES ($1, $2, $3) RETURNING id`,
		req.Name, req.ListID, pq.Array(req.ExitListIDs)); err != nil {
		return seqDBErr(a, "creating sequence", err)
	}

	out, err := a.getSequence(id)
	if err != nil {
		return err
	}

	return c.JSON(http.StatusOK, okResp{out})
}

// UpdateSequence updates a sequence's name, list and exit lists. The list can
// only be changed while the sequence has no steps, as every step's e-mail is
// tied to it.
func (a *App) UpdateSequence(c echo.Context) error {
	id := getID(c)

	var req sequenceReq
	if err := c.Bind(&req); err != nil {
		return err
	}
	if err := a.validateSequenceReq(&req); err != nil {
		return err
	}

	cur, err := a.getSequence(id)
	if err != nil {
		return err
	}
	if cur.ListID != req.ListID && len(cur.Steps) > 0 {
		return echo.NewHTTPError(http.StatusBadRequest,
			"The list can't be changed once the sequence has steps. Remove the steps first.")
	}

	if _, err := a.db.Exec(`UPDATE sequences SET name = $2, list_id = $3, exit_list_ids = $4, updated_at = NOW() WHERE id = $1`,
		id, req.Name, req.ListID, pq.Array(req.ExitListIDs)); err != nil {
		return seqDBErr(a, "updating sequence", err)
	}

	out, err := a.getSequence(id)
	if err != nil {
		return err
	}

	return c.JSON(http.StatusOK, okResp{out})
}

// UpdateSequenceStatus switches a sequence on or off. The first time it is
// switched on, that moment becomes the cut-off: only people who join the list
// after it are enrolled, so switching a sequence on never e-mails the people
// already on the list.
func (a *App) UpdateSequenceStatus(c echo.Context) error {
	id := getID(c)

	req := struct {
		Status string `json:"status"`
	}{}
	if err := c.Bind(&req); err != nil {
		return err
	}
	if req.Status != seqStatusActive && req.Status != seqStatusPaused {
		return echo.NewHTTPError(http.StatusBadRequest, "Invalid status.")
	}

	cur, err := a.getSequence(id)
	if err != nil {
		return err
	}
	if req.Status == seqStatusActive && len(cur.Steps) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "Add at least one step before switching the sequence on.")
	}

	if _, err := a.db.Exec(`UPDATE sequences SET status = $2, last_error = '',
		activated_at = (CASE WHEN $2 = 'active' THEN COALESCE(activated_at, NOW()) ELSE activated_at END),
		updated_at = NOW() WHERE id = $1`, id, req.Status); err != nil {
		return seqDBErr(a, "updating sequence", err)
	}

	out, err := a.getSequence(id)
	if err != nil {
		return err
	}

	return c.JSON(http.StatusOK, okResp{out})
}

// DeleteSequence deletes a sequence, its steps and its history. The step
// e-mails themselves stay as ordinary draft campaigns.
func (a *App) DeleteSequence(c echo.Context) error {
	if _, err := a.db.Exec(`DELETE FROM sequences WHERE id = $1`, getID(c)); err != nil {
		return seqDBErr(a, "deleting sequence", err)
	}

	return c.JSON(http.StatusOK, okResp{true})
}

// CreateSequenceStep adds a campaign to the end of a sequence as a new step.
func (a *App) CreateSequenceStep(c echo.Context) error {
	id := getID(c)

	var req sequenceStepReq
	if err := c.Bind(&req); err != nil {
		return err
	}
	if req.DelayMinutes < 0 || req.DelayMinutes > seqMaxDelayMinutes {
		return echo.NewHTTPError(http.StatusBadRequest, "The wait must be between 0 minutes and 1 year.")
	}

	seq, err := a.getSequence(id)
	if err != nil {
		return err
	}
	if err := a.validateStepCampaign(seq, req.CampaignID); err != nil {
		return err
	}

	if _, err := a.db.Exec(`INSERT INTO sequence_steps (sequence_id, campaign_id, delay_minutes, position)
		VALUES ($1, $2, $3, (SELECT COALESCE(MAX(position), 0) + 1 FROM sequence_steps WHERE sequence_id = $1))`,
		id, req.CampaignID, req.DelayMinutes); err != nil {
		if strings.Contains(err.Error(), "sequence_steps_campaign_id_key") {
			return echo.NewHTTPError(http.StatusBadRequest, "That e-mail is already a step in a sequence.")
		}
		return seqDBErr(a, "adding step", err)
	}

	out, err := a.getSequence(id)
	if err != nil {
		return err
	}

	return c.JSON(http.StatusOK, okResp{out})
}

// UpdateSequenceStep changes the wait before a step.
func (a *App) UpdateSequenceStep(c echo.Context) error {
	var (
		id        = getID(c)
		stepID, _ = strconv.Atoi(c.Param("stepID"))
	)

	var req sequenceStepReq
	if err := c.Bind(&req); err != nil {
		return err
	}
	if req.DelayMinutes < 0 || req.DelayMinutes > seqMaxDelayMinutes {
		return echo.NewHTTPError(http.StatusBadRequest, "The wait must be between 0 minutes and 1 year.")
	}

	if _, err := a.db.Exec(`UPDATE sequence_steps SET delay_minutes = $3, updated_at = NOW()
		WHERE id = $2 AND sequence_id = $1`, id, stepID, req.DelayMinutes); err != nil {
		return seqDBErr(a, "updating step", err)
	}

	out, err := a.getSequence(id)
	if err != nil {
		return err
	}

	return c.JSON(http.StatusOK, okResp{out})
}

// OrderSequenceSteps sets the order of a sequence's steps from a list of step IDs.
func (a *App) OrderSequenceSteps(c echo.Context) error {
	id := getID(c)

	req := struct {
		IDs []int `json:"ids"`
	}{}
	if err := c.Bind(&req); err != nil {
		return err
	}

	seq, err := a.getSequence(id)
	if err != nil {
		return err
	}
	if len(req.IDs) != len(seq.Steps) {
		return echo.NewHTTPError(http.StatusBadRequest, "The order must include every step in the sequence.")
	}
	have := map[int]bool{}
	for _, s := range seq.Steps {
		have[s.ID] = true
	}
	for _, sid := range req.IDs {
		if !have[sid] {
			return echo.NewHTTPError(http.StatusBadRequest, "The order must include every step in the sequence.")
		}
		delete(have, sid)
	}

	tx, err := a.db.Beginx()
	if err != nil {
		return seqDBErr(a, "ordering steps", err)
	}
	defer tx.Rollback()

	for i, sid := range req.IDs {
		if _, err := tx.Exec(`UPDATE sequence_steps SET position = $3, updated_at = NOW() WHERE id = $2 AND sequence_id = $1`,
			id, sid, i+1); err != nil {
			return seqDBErr(a, "ordering steps", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return seqDBErr(a, "ordering steps", err)
	}

	out, err := a.getSequence(id)
	if err != nil {
		return err
	}

	return c.JSON(http.StatusOK, okResp{out})
}

// DeleteSequenceStep removes a step from a sequence and closes the gap in the
// order. The step's e-mail stays as an ordinary draft campaign. People who
// were already past the removed step keep their place.
func (a *App) DeleteSequenceStep(c echo.Context) error {
	var (
		id        = getID(c)
		stepID, _ = strconv.Atoi(c.Param("stepID"))
	)

	tx, err := a.db.Beginx()
	if err != nil {
		return seqDBErr(a, "removing step", err)
	}
	defer tx.Rollback()

	var pos int
	if err := tx.Get(&pos, `DELETE FROM sequence_steps WHERE id = $2 AND sequence_id = $1 RETURNING position`, id, stepID); err != nil {
		if err == sql.ErrNoRows {
			return echo.NewHTTPError(http.StatusBadRequest, "Step not found.")
		}
		return seqDBErr(a, "removing step", err)
	}
	if _, err := tx.Exec(`UPDATE sequence_steps SET position = position - 1 WHERE sequence_id = $1 AND position > $2`, id, pos); err != nil {
		return seqDBErr(a, "removing step", err)
	}
	if _, err := tx.Exec(`UPDATE sequence_enrolments SET last_position = last_position - 1
		WHERE sequence_id = $1 AND last_position >= $2`, id, pos); err != nil {
		return seqDBErr(a, "removing step", err)
	}
	if err := tx.Commit(); err != nil {
		return seqDBErr(a, "removing step", err)
	}

	out, err := a.getSequence(id)
	if err != nil {
		return err
	}

	return c.JSON(http.StatusOK, okResp{out})
}

// RemoveSequenceSubscriber takes one person out of a sequence so they get no
// further steps. They stay on the list.
func (a *App) RemoveSequenceSubscriber(c echo.Context) error {
	var (
		id       = getID(c)
		subID, _ = strconv.Atoi(c.Param("subID"))
	)

	if _, err := a.db.Exec(`UPDATE sequence_enrolments SET status = 'exited', exit_reason = 'removed by hand', updated_at = NOW()
		WHERE sequence_id = $1 AND subscriber_id = $2 AND status = 'active'`, id, subID); err != nil {
		return seqDBErr(a, "removing subscriber from sequence", err)
	}

	return c.JSON(http.StatusOK, okResp{true})
}

// getSequence fetches a sequence and its steps.
func (a *App) getSequence(id int) (sequence, error) {
	var out sequence
	if err := a.db.Get(&out, seqSelect+` WHERE s.id = $1`, id); err != nil {
		if err == sql.ErrNoRows {
			return out, echo.NewHTTPError(http.StatusBadRequest, "Sequence not found.")
		}
		return out, seqDBErr(a, "fetching sequence", err)
	}

	out.Steps = []sequenceStep{}
	if err := a.db.Select(&out.Steps, `
		SELECT st.id, st.sequence_id, st.campaign_id, c.name AS campaign_name, c.subject AS campaign_subject,
			st.position, st.delay_minutes,
			(SELECT COUNT(*) FROM sequence_sends sn WHERE sn.step_id = st.id) AS num_sent,
			(SELECT COUNT(*) FROM campaign_views v WHERE v.campaign_id = st.campaign_id) AS num_views,
			(SELECT COUNT(*) FROM link_clicks k WHERE k.campaign_id = st.campaign_id) AS num_clicks
		FROM sequence_steps st JOIN campaigns c ON c.id = st.campaign_id
		WHERE st.sequence_id = $1 ORDER BY st.position, st.id`, id); err != nil {
		return out, seqDBErr(a, "fetching sequence steps", err)
	}

	return out, nil
}

// validateSequenceReq validates and tidies an incoming sequence.
func (a *App) validateSequenceReq(req *sequenceReq) error {
	req.Name = strings.TrimSpace(req.Name)
	if !strHasLen(req.Name, 1, stdInputMaxLen) {
		return echo.NewHTTPError(http.StatusBadRequest, "Give the sequence a name.")
	}
	if req.ListID < 1 {
		return echo.NewHTTPError(http.StatusBadRequest, "Choose the list that starts the sequence.")
	}

	var n int
	if err := a.db.Get(&n, `SELECT COUNT(*) FROM lists WHERE id = $1`, req.ListID); err != nil {
		return seqDBErr(a, "checking list", err)
	}
	if n == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "The chosen list doesn't exist.")
	}

	// The starting list can't also be an exit list.
	exit := make([]int, 0, len(req.ExitListIDs))
	for _, id := range req.ExitListIDs {
		if id > 0 && id != req.ListID {
			exit = append(exit, id)
		}
	}
	req.ExitListIDs = exit

	return nil
}

// validateStepCampaign checks that a campaign can be used as a step: it has
// to be an unsent regular campaign that targets the sequence's list. The
// list matters because a campaign's unsubscribe link unsubscribes the person
// from that campaign's lists, and that is what stops the sequence for them.
func (a *App) validateStepCampaign(seq sequence, campID int) error {
	if campID < 1 {
		return echo.NewHTTPError(http.StatusBadRequest, "Choose the e-mail for this step.")
	}

	camp := struct {
		Status  string `db:"status"`
		Type    string `db:"type"`
		HasList bool   `db:"has_list"`
	}{}
	if err := a.db.Get(&camp, `SELECT status::TEXT AS status, type::TEXT AS type,
		EXISTS (SELECT 1 FROM campaign_lists cl WHERE cl.campaign_id = c.id AND cl.list_id = $2) AS has_list
		FROM campaigns c WHERE c.id = $1`, campID, seq.ListID); err != nil {
		if err == sql.ErrNoRows {
			return echo.NewHTTPError(http.StatusBadRequest, "That e-mail doesn't exist.")
		}
		return seqDBErr(a, "checking campaign", err)
	}

	if camp.Type != models.CampaignTypeRegular || camp.Status != models.CampaignStatusDraft {
		return echo.NewHTTPError(http.StatusBadRequest, "Only draft campaigns can be used as a step.")
	}
	if !camp.HasList {
		return echo.NewHTTPError(http.StatusBadRequest,
			fmt.Sprintf("That e-mail isn't addressed to the list \"%s\". Add the list to it first, so its unsubscribe link works for this sequence.", seq.ListName))
	}

	return nil
}

// runSequences is one pass of the engine over every active sequence.
func (a *App) runSequences() {
	defer func() {
		if r := recover(); r != nil {
			a.log.Printf("sequences: recovered from panic: %v", r)
		}
	}()

	seqs := []sequence{}
	if err := a.db.Select(&seqs, seqSelect+` WHERE s.status = 'active' AND s.activated_at IS NOT NULL ORDER BY s.id`); err != nil {
		a.log.Printf("sequences: error fetching active sequences: %v", err)
		return
	}

	for _, s := range seqs {
		if err := a.runSequence(s); err != nil {
			a.log.Printf("sequences: error running \"%s\": %v", s.Name, err)
			a.setSequenceError(s.ID, err.Error())
		} else if s.LastError != "" {
			a.setSequenceError(s.ID, "")
		}
	}
}

// runSequence enrols new joiners, takes out people who have left, and sends
// the steps that are due for one sequence.
func (a *App) runSequence(s sequence) error {
	// Enrol people who joined the list after the sequence was switched on.
	// Who counts as subscribed mirrors how campaigns pick their recipients.
	if _, err := a.db.Exec(`
		INSERT INTO sequence_enrolments (sequence_id, subscriber_id)
		SELECT $1, sl.subscriber_id FROM subscriber_lists sl
			JOIN subscribers sub ON sub.id = sl.subscriber_id
			JOIN lists l ON l.id = sl.list_id
		WHERE sl.list_id = $2 AND sl.created_at >= $3 AND sub.status != 'blocklisted'
			AND ((l.optin = 'double' AND sl.status = 'confirmed') OR (l.optin != 'double' AND sl.status != 'unsubscribed'))
		ON CONFLICT (sequence_id, subscriber_id) DO NOTHING`,
		s.ID, s.ListID, s.ActivatedAt.Time); err != nil {
		return fmt.Errorf("enrolling: %v", err)
	}

	// Take out anyone who has unsubscribed, been blocklisted, left the list
	// or been added to one of the exit lists.
	if _, err := a.db.Exec(`
		WITH x AS (
			SELECT e.id, (CASE
				WHEN sub.status = 'blocklisted' THEN 'blocklisted'
				WHEN sl.subscriber_id IS NULL THEN 'removed from the list'
				WHEN sl.status = 'unsubscribed' THEN 'unsubscribed'
				WHEN EXISTS (
					SELECT 1 FROM subscriber_lists xl WHERE xl.subscriber_id = e.subscriber_id
					AND xl.list_id = ANY($3) AND xl.status != 'unsubscribed'
				) THEN 'added to an exit list'
			END) AS reason
			FROM sequence_enrolments e
			JOIN subscribers sub ON sub.id = e.subscriber_id
			LEFT JOIN subscriber_lists sl ON sl.subscriber_id = e.subscriber_id AND sl.list_id = $2
			WHERE e.sequence_id = $1 AND e.status = 'active'
		)
		UPDATE sequence_enrolments e SET status = 'exited', exit_reason = x.reason, updated_at = NOW()
		FROM x WHERE e.id = x.id AND x.reason IS NOT NULL`,
		s.ID, s.ListID, s.ExitListIDs); err != nil {
		return fmt.Errorf("checking exits: %v", err)
	}

	// Mark people who have had the last step as done.
	if _, err := a.db.Exec(`
		UPDATE sequence_enrolments e SET status = 'completed', updated_at = NOW()
		WHERE e.sequence_id = $1 AND e.status = 'active'
			AND e.last_position >= (SELECT MAX(position) FROM sequence_steps WHERE sequence_id = $1)`,
		s.ID); err != nil {
		return fmt.Errorf("completing: %v", err)
	}

	// Each person's next step, where its wait has passed.
	due := []struct {
		EnrolmentID  int64 `db:"enrolment_id"`
		SubscriberID int   `db:"subscriber_id"`
		StepID       int   `db:"step_id"`
		CampaignID   int   `db:"campaign_id"`
		Position     int   `db:"position"`
	}{}
	if err := a.db.Select(&due, `
		SELECT e.id AS enrolment_id, e.subscriber_id, st.id AS step_id, st.campaign_id, st.position
		FROM sequence_enrolments e
		JOIN LATERAL (
			SELECT id, campaign_id, position, delay_minutes FROM sequence_steps
			WHERE sequence_id = e.sequence_id AND position > e.last_position
			ORDER BY position LIMIT 1
		) st ON TRUE
		WHERE e.sequence_id = $1 AND e.status = 'active'
			AND COALESCE(e.last_sent_at, e.enrolled_at) + (st.delay_minutes * INTERVAL '1 minute') <= NOW()
		ORDER BY e.id LIMIT $2`, s.ID, seqBatchSize); err != nil {
		return fmt.Errorf("finding due steps: %v", err)
	}
	if len(due) == 0 {
		return nil
	}

	// Compiled step e-mails, kept for the length of this pass.
	camps := map[int]*models.Campaign{}

	var lastErr error
	for _, d := range due {
		// Record the send first. The unique (step, subscriber) key is what
		// guarantees nobody is sent the same step twice.
		res, err := a.db.Exec(`INSERT INTO sequence_sends (sequence_id, step_id, subscriber_id, campaign_id)
			VALUES ($1, $2, $3, $4) ON CONFLICT (step_id, subscriber_id) DO NOTHING`,
			s.ID, d.StepID, d.SubscriberID, d.CampaignID)
		if err != nil {
			lastErr = fmt.Errorf("recording send: %v", err)
			continue
		}

		// They already have this step (the steps were re-ordered). Move on
		// to the next one without sending or restarting the wait.
		if n, _ := res.RowsAffected(); n == 0 {
			if _, err := a.db.Exec(`UPDATE sequence_enrolments SET last_position = $2, updated_at = NOW() WHERE id = $1`,
				d.EnrolmentID, d.Position); err != nil {
				lastErr = fmt.Errorf("advancing enrolment: %v", err)
			}
			continue
		}

		if err := a.sendSequenceStep(s, d.CampaignID, d.SubscriberID, camps); err != nil {
			// Undo the record so the step is retried on the next pass.
			if _, dErr := a.db.Exec(`DELETE FROM sequence_sends WHERE step_id = $1 AND subscriber_id = $2`,
				d.StepID, d.SubscriberID); dErr != nil {
				a.log.Printf("sequences: error undoing send record: %v", dErr)
			}
			lastErr = err
			continue
		}

		if _, err := a.db.Exec(`UPDATE sequence_enrolments SET last_position = $2, last_sent_at = NOW(), updated_at = NOW() WHERE id = $1`,
			d.EnrolmentID, d.Position); err != nil {
			lastErr = fmt.Errorf("advancing enrolment: %v", err)
		}
	}

	return lastErr
}

// sendSequenceStep renders a step's campaign for one subscriber and queues it
// on the campaign manager, the same way a campaign test send does. It goes
// out through the normal sending workers, so the instance's rate limit applies.
func (a *App) sendSequenceStep(s sequence, campID, subID int, camps map[int]*models.Campaign) error {
	camp, ok := camps[campID]
	if !ok {
		// The e-mail has to still be addressed to the sequence's list, or its
		// unsubscribe link would not stop the sequence.
		var hasList bool
		if err := a.db.Get(&hasList, `SELECT EXISTS (SELECT 1 FROM campaign_lists WHERE campaign_id = $1 AND list_id = $2)`,
			campID, s.ListID); err != nil {
			return fmt.Errorf("checking step e-mail: %v", err)
		}

		c, err := a.core.GetCampaign(campID, "", "")
		if err != nil {
			return fmt.Errorf("loading step e-mail: %v", err)
		}
		if !hasList {
			return fmt.Errorf("the e-mail \"%s\" is no longer addressed to the list \"%s\". Add the list back to it", c.Name, s.ListName)
		}
		if !a.manager.HasMessenger(c.Messenger) {
			return fmt.Errorf("the e-mail \"%s\" uses a messenger that isn't set up (%s)", c.Name, c.Messenger)
		}
		if err := a.manager.LoadInlineImages(&c); err != nil {
			return fmt.Errorf("loading images for \"%s\": %v", c.Name, err)
		}
		if err := c.CompileTemplate(a.manager.TemplateFuncs(&c)); err != nil {
			return fmt.Errorf("the e-mail \"%s\" has a template error: %v", c.Name, err)
		}

		camp = &c
		camps[campID] = camp
	}

	sub, err := a.core.GetSubscriber(subID, "", "")
	if err != nil {
		return fmt.Errorf("loading subscriber %d: %v", subID, err)
	}

	msg, err := a.manager.NewCampaignMessage(camp, sub)
	if err != nil {
		return fmt.Errorf("the e-mail \"%s\" could not be rendered: %v", camp.Name, err)
	}

	if err := a.manager.PushCampaignMessage(msg); err != nil {
		return fmt.Errorf("queueing \"%s\": %v", camp.Name, err)
	}

	return nil
}

// setSequenceError records the latest problem on a sequence so it shows in the UI.
func (a *App) setSequenceError(id int, msg string) {
	if len(msg) > 500 {
		msg = msg[:500]
	}
	if _, err := a.db.Exec(`UPDATE sequences SET last_error = $2 WHERE id = $1`, id, msg); err != nil {
		a.log.Printf("sequences: error recording error: %v", err)
	}
}

// seqDBErr logs a DB error and returns a generic HTTP error for it.
func seqDBErr(a *App, doing string, err error) error {
	a.log.Printf("sequences: error %s: %v", doing, err)
	return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("Error %s: %v", doing, err))
}
