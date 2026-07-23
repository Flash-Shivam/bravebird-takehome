// Package api is the HTTP control surface: submit jobs, poll status,
// tail logs, fetch pre-signed artifact URLs. Stdlib net/http only.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/oklog/ulid/v2"

	"github.com/shivamjadhav/bravebird-takehome/internal/auth"
	"github.com/shivamjadhav/bravebird-takehome/internal/store"
)

type Config struct {
	QueueHighURL   string
	QueueLowURL    string
	Bucket         string
	AgentLogGroup  string
	RatePerMinute  int
	JWTSecret      []byte
}

type Server struct {
	store *store.Store
	sqs   *sqs.Client
	s3    *s3.Client
	logs  *cloudwatchlogs.Client
	cfg   Config
}

func NewServer(st *store.Store, sq *sqs.Client, s3c *s3.Client, cw *cloudwatchlogs.Client, cfg Config) *Server {
	return &Server{store: st, sqs: sq, s3: s3c, logs: cw, cfg: cfg}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /jobs", s.createJob)
	mux.HandleFunc("GET /jobs/{id}", s.getJob)
	mux.HandleFunc("GET /jobs/{id}/logs", s.getLogs)
	mux.HandleFunc("GET /jobs/{id}/artifacts", s.getArtifacts)

	outer := http.NewServeMux()
	outer.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	outer.Handle("/", s.requireAuth(mux))
	return outer
}

type ctxKey struct{}

// requireAuth validates the Bearer JWT and stashes the verified subject in the
// request context. Everything behind it can trust userID(r).
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			writeErr(w, http.StatusUnauthorized, "Authorization: Bearer <token> required")
			return
		}
		sub, err := auth.Verify(s.cfg.JWTSecret, token)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "invalid token")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, sub)))
	})
}

func userID(r *http.Request) string {
	sub, _ := r.Context().Value(ctxKey{}).(string)
	return sub
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

type createReq struct {
	Prompt   string `json:"prompt"`
	Priority string `json:"priority"`
}

func (s *Server) createJob(w http.ResponseWriter, r *http.Request) {
	userID := userID(r)
	var req createReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.Prompt = strings.TrimSpace(req.Prompt)
	if req.Prompt == "" || len(req.Prompt) > 1024 {
		writeErr(w, http.StatusBadRequest, "prompt required (max 1024 chars)")
		return
	}
	if req.Priority == "" {
		req.Priority = "low"
	}
	if req.Priority != "high" && req.Priority != "low" {
		writeErr(w, http.StatusBadRequest, `priority must be "high" or "low"`)
		return
	}

	ok, err := s.store.AllowRequest(r.Context(), userID, s.cfg.RatePerMinute)
	if err != nil {
		slog.Error("rate limit check failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !ok {
		writeErr(w, http.StatusTooManyRequests, fmt.Sprintf("rate limit exceeded (%d/min per user)", s.cfg.RatePerMinute))
		return
	}

	job := store.Job{
		ID:        ulid.Make().String(),
		UserID:    userID,
		Prompt:    req.Prompt,
		Priority:  req.Priority,
		Status:    store.StatusQueued,
		CreatedAt: time.Now().Unix(),
	}
	if err := s.store.CreateJob(r.Context(), job); err != nil {
		slog.Error("create job failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}

	queueURL := s.cfg.QueueLowURL
	if job.Priority == "high" {
		queueURL = s.cfg.QueueHighURL
	}
	// Message carries only the job id; the dispatcher reads the record from DDB.
	if _, err := s.sqs.SendMessage(r.Context(), &sqs.SendMessageInput{
		QueueUrl:    &queueURL,
		MessageBody: &job.ID,
	}); err != nil {
		slog.Error("enqueue failed", "err", err, "job", job.ID)
		_ = s.store.Finish(r.Context(), job.ID, store.StatusFailed, "enqueue failed")
		writeErr(w, http.StatusInternalServerError, "failed to enqueue job")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"job_id": job.ID, "status": job.Status})
}

func (s *Server) loadJob(w http.ResponseWriter, r *http.Request) *store.Job {
	job, err := s.store.GetJob(r.Context(), r.PathValue("id"))
	if err != nil {
		slog.Error("get job failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return nil
	}
	// Ownership check; 404 (not 403) so job IDs don't leak existence.
	if job == nil || job.UserID != userID(r) {
		writeErr(w, http.StatusNotFound, "job not found")
		return nil
	}
	return job
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	if job := s.loadJob(w, r); job != nil {
		writeJSON(w, http.StatusOK, job)
	}
}

// getLogs tails the agent's CloudWatch log stream. Poll with ?since=<next_since>
// from the previous response for a follow-style tail.
func (s *Server) getLogs(w http.ResponseWriter, r *http.Request) {
	job := s.loadJob(w, r)
	if job == nil {
		return
	}
	if job.TaskARN == "" {
		writeJSON(w, http.StatusOK, map[string]any{"events": []any{}, "next_since": 0, "note": "task not launched yet"})
		return
	}
	// awslogs stream name: {prefix}/{container}/{task-id}
	taskID := job.TaskARN[strings.LastIndex(job.TaskARN, "/")+1:]
	stream := "agent/agent/" + taskID

	input := &cloudwatchlogs.GetLogEventsInput{
		LogGroupName:  &s.cfg.AgentLogGroup,
		LogStreamName: &stream,
		StartFromHead: aws.Bool(true),
	}
	if since := r.URL.Query().Get("since"); since != "" {
		ms, err := strconv.ParseInt(since, 10, 64)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "since must be a unix-millis integer")
			return
		}
		input.StartTime = &ms
	}
	out, err := s.logs.GetLogEvents(r.Context(), input)
	if err != nil {
		var rnf *cwtypes.ResourceNotFoundException
		if errors.As(err, &rnf) {
			writeJSON(w, http.StatusOK, map[string]any{"events": []any{}, "next_since": 0, "note": "log stream not created yet"})
			return
		}
		slog.Error("get log events failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	type event struct {
		Timestamp int64  `json:"ts"`
		Message   string `json:"message"`
	}
	events := make([]event, 0, len(out.Events))
	var nextSince int64
	for _, e := range out.Events {
		events = append(events, event{Timestamp: *e.Timestamp, Message: strings.TrimRight(*e.Message, "\n")})
		if *e.Timestamp >= nextSince {
			nextSince = *e.Timestamp + 1
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events, "next_since": nextSince})
}

// getArtifacts lists the job's S3 objects and returns 15-minute pre-signed URLs.
func (s *Server) getArtifacts(w http.ResponseWriter, r *http.Request) {
	job := s.loadJob(w, r)
	if job == nil {
		return
	}
	prefix := job.ID + "/"
	out, err := s.s3.ListObjectsV2(r.Context(), &s3.ListObjectsV2Input{
		Bucket: &s.cfg.Bucket,
		Prefix: &prefix,
	})
	if err != nil {
		slog.Error("list artifacts failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	presigner := s3.NewPresignClient(s.s3)
	type artifact struct {
		Key  string `json:"key"`
		Size int64  `json:"size"`
		URL  string `json:"url"`
	}
	artifacts := make([]artifact, 0, len(out.Contents))
	for _, obj := range out.Contents {
		req, err := presigner.PresignGetObject(r.Context(), &s3.GetObjectInput{
			Bucket: &s.cfg.Bucket,
			Key:    obj.Key,
		}, s3.WithPresignExpires(15*time.Minute))
		if err != nil {
			slog.Error("presign failed", "err", err, "key", *obj.Key)
			continue
		}
		artifacts = append(artifacts, artifact{Key: *obj.Key, Size: *obj.Size, URL: req.URL})
	}
	writeJSON(w, http.StatusOK, map[string]any{"job_id": job.ID, "artifacts": artifacts})
}
