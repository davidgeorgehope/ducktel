package receiver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/davidgeorgehope/ducktel/internal/feedback"
	"github.com/davidgeorgehope/ducktel/internal/writer"
)

const maxFeedbackBody = 64 << 10

func (r *Receiver) writeFeedback(ctx context.Context, report feedback.Report) error {
	if r.flush == nil {
		return errors.New("flush is not configured")
	}
	r.consumer.AddLogs([]writer.LogRecord{report.LogRecord(time.Now())})
	return r.flush()
}

func (r *Receiver) handleFeedback(w http.ResponseWriter, req *http.Request) {
	if r.feedback == nil {
		http.Error(w, "feedback storage is not configured", http.StatusInternalServerError)
		return
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, maxFeedbackBody+1))
	if err != nil {
		http.Error(w, "reading feedback body", http.StatusBadRequest)
		return
	}
	if len(body) > maxFeedbackBody {
		http.Error(w, "feedback body exceeds 64 KiB", http.StatusRequestEntityTooLarge)
		return
	}
	res, err := r.feedback.SubmitJSON(req.Context(), body)
	if err != nil {
		status := http.StatusInternalServerError
		var fe *feedback.FieldError
		switch {
		case errors.As(err, &fe):
			status = http.StatusBadRequest
		case errors.Is(err, feedback.ErrRateLimited):
			status = http.StatusTooManyRequests
		}
		http.Error(w, err.Error(), status)
		return
	}
	log.Printf("feedback: stored report_id=%s", res.ReportID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(struct {
		ReportID string `json:"report_id"`
		Status   string `json:"status"`
	}{res.ReportID, "stored"})
}
