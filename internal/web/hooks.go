package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/kbelokon/readout/internal/config"
	"github.com/kbelokon/readout/internal/kube"
	"golang.org/x/oauth2"
)

// authorizationHook consults the configured JSON authorization hook, updating
// session in place with any user/email/groups the hook returns. session is taken
// by pointer (it is a heavy value) and mutated directly; the caller reads the
// updated session after the call. Returns whether access is allowed.
func (s *Server) authorizationHook(ctx context.Context, token *oauth2.Token, session *authSession) (bool, error) {
	if s.cfg.AuthorizationHookURL == "" {
		return true, nil
	}
	payload := struct {
		Token   map[string]any `json:"token"`
		Session authSession    `json:"session"`
	}{
		Token: map[string]any{
			"access_token":  token.AccessToken,
			"token_type":    token.TokenType,
			"refresh_token": token.RefreshToken,
			"expiry":        token.Expiry.Format(time.RFC3339),
		},
		Session: *session,
	}
	if idToken, _ := token.Extra("id_token").(string); idToken != "" {
		payload.Token["id_token"] = idToken
	}
	var response struct {
		Allowed *bool    `json:"allowed"`
		User    string   `json:"user"`
		Email   string   `json:"email"`
		Groups  []string `json:"groups"`
	}
	if err := postJSON(ctx, s.cfg.AuthorizationHookURL, payload, &response); err != nil {
		return false, err
	}
	if response.User != "" {
		session.User = response.User
	}
	if response.Email != "" {
		session.Email = response.Email
	}
	if response.Groups != nil {
		session.Groups = response.Groups
	}
	if response.Allowed != nil {
		return *response.Allowed, nil
	}
	return true, nil
}

func (s *Server) resourcePrerenderHook(ctx context.Context, cluster, namespace, plural string, object *kube.Object, links []config.Link) ([]config.Link, map[string]any, error) {
	if s.cfg.ResourcePrerenderHookURL == "" {
		return links, nil, nil
	}
	payload := struct {
		Cluster   string         `json:"cluster"`
		Namespace string         `json:"namespace"`
		Plural    string         `json:"plural"`
		Resource  map[string]any `json:"resource"`
		Links     []config.Link  `json:"links"`
	}{
		Cluster:   cluster,
		Namespace: namespace,
		Plural:    plural,
		Resource:  object.Raw,
		Links:     links,
	}
	var response struct {
		Links    []config.Link  `json:"links"`
		Resource map[string]any `json:"resource"`
	}
	if err := postJSON(ctx, s.cfg.ResourcePrerenderHookURL, payload, &response); err != nil {
		return nil, nil, err
	}
	if len(response.Links) > 0 {
		links = append(links, response.Links...)
	}
	return links, response.Resource, nil
}

func postJSON(ctx context.Context, endpoint string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s: %s", resp.Status, string(data))
	}
	if len(data) == 0 || out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}
