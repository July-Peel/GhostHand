package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"ghosthand/launcher/state"
	"ghosthand/proto"
)

func (a *agent) uploadScreenshot(ctx context.Context, task, upload string, data []byte, wake <-chan struct{}) (proto.ScreenshotResult, error) {
	var result proto.ScreenshotResult
	err := retry(ctx, func() error {
		attempt, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		ticket, err := a.screenshotTicket(attempt, task, upload, wake)
		if err != nil {
			return err
		}
		if ticket.AttachmentID != "" {
			result.AttachmentID = ticket.AttachmentID
			return nil
		}
		cfg := a.cfg.Load()
		req, err := http.NewRequestWithContext(attempt, http.MethodPost, cfg.Server+"/api/upload", bytes.NewReader(data))
		if err != nil {
			return permanentError{err}
		}
		req.Header.Set("Authorization", "Bearer "+ticket.Token)
		req.Header.Set("X-GhostHand-Task", task)
		if upload != "" {
			req.Header.Set("X-GhostHand-Upload", upload)
		}
		req.Header.Set("Content-Type", "image/jpeg")
		response, err := a.client.Do(req)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.StatusCode == http.StatusUnauthorized {
			return errors.New("screenshot ticket expired or already redeemed")
		}
		if response.StatusCode != http.StatusCreated {
			return statusError(response.StatusCode)
		}
		if err := json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&result); err != nil {
			return err
		}
		if !state.IDPattern.MatchString(result.AttachmentID) {
			return errors.New("invalid screenshot upload response")
		}
		return nil
	})
	return result, err
}

func (a *agent) screenshotTicket(ctx context.Context, id, upload string, wake <-chan struct{}) (proto.ScreenshotTicket, error) {
	requestID, err := proto.NewID()
	if err != nil {
		return proto.ScreenshotTicket{}, err
	}
	err = a.local.changeTask(id, func(task *localTask) error {
		task.TicketRequest, task.Ticket = requestID, proto.ScreenshotTicket{}
		return a.local.enqueueInLock(proto.UploadRequest, proto.ScreenshotRequest{TaskID: id, RequestID: requestID, UploadID: upload})
	})
	if err != nil {
		return proto.ScreenshotTicket{}, err
	}
	for {
		tasks, err := a.local.tasks()
		if err != nil {
			return proto.ScreenshotTicket{}, err
		}
		for _, task := range tasks {
			if task.TaskID == id && task.Ticket.RequestID == requestID && task.Ticket.UploadID == upload {
				return task.Ticket, nil
			}
		}
		select {
		case <-ctx.Done():
			return proto.ScreenshotTicket{}, ctx.Err()
		case <-wake:
		}
	}
}
