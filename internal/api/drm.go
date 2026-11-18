package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/5centscdn/origin-mediaserver/internal/drm"
	"github.com/5centscdn/origin-mediaserver/internal/streamoutputs"
)

// Per-application castLabs DRMtoday settings (internal/drm), carried as a
// nested "drm" object on the application endpoints. The API password is
// write-only: reads return has_password, like push_password.

type appDRMView struct {
	Enabled      bool   `json:"enabled"`
	Staging      bool   `json:"staging"`
	User         string `json:"user"`
	HasPassword  bool   `json:"has_password"`
	MerchantUUID string `json:"merchant_uuid"`
	CPIXConfigID string `json:"cpix_config_id"`
	AssetID      string `json:"asset_id"`
}

// appDRMUpdate is the "drm" object of a create/update; nil fields keep the
// stored value (a password the panel never shows isn't wiped).
type appDRMUpdate struct {
	Enabled      *bool   `json:"enabled"`
	Staging      *bool   `json:"staging"`
	User         *string `json:"user"`
	Password     *string `json:"password"`
	MerchantUUID *string `json:"merchant_uuid"`
	CPIXConfigID *string `json:"cpix_config_id"`
	AssetID      *string `json:"asset_id"`
}

// loadAppDRM returns every application's DRM view by id.
func (s *Server) loadAppDRM(ctx context.Context) map[string]appDRMView {
	out := map[string]appDRMView{}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, drm_enabled, drm_staging, drm_user, drm_password != '',
		       drm_merchant_uuid, drm_cpix_config_id, drm_asset_id
		FROM applications`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var v appDRMView
		var en, st int
		if rows.Scan(&id, &en, &st, &v.User, &v.HasPassword, &v.MerchantUUID, &v.CPIXConfigID, &v.AssetID) == nil {
			v.Enabled, v.Staging = en == 1, st == 1
			out[id] = v
		}
	}
	return out
}

// applyAppDRM stores u for application id. Enabling DRM requires the full
// DRMtoday account, so a stream never starts waiting on settings that
// can't work. Cached keys of the old settings are dropped.
func (s *Server) applyAppDRM(ctx context.Context, id string, u *appDRMUpdate) error {
	if u == nil {
		return nil
	}
	var name string
	if err := s.db.QueryRowContext(ctx, `SELECT name FROM applications WHERE id = ?`, id).Scan(&name); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("application not found")
		}
		return err
	}
	old, err := s.db.GetApplicationByName(ctx, name)
	if err != nil || old == nil {
		return errors.New("application not found")
	}
	next := *old
	set := func(dst *string, v *string) {
		if v != nil {
			*dst = strings.TrimSpace(*v)
		}
	}
	if u.Enabled != nil {
		next.DRMEnabled = *u.Enabled
	}
	if u.Staging != nil {
		next.DRMStaging = *u.Staging
	}
	set(&next.DRMUser, u.User)
	if u.Password != nil {
		next.DRMPassword = *u.Password
	}
	set(&next.DRMMerchantUUID, u.MerchantUUID)
	set(&next.DRMCPIXConfigID, u.CPIXConfigID)
	set(&next.DRMAssetID, u.AssetID)
	if next.DRMEnabled {
		if err := streamoutputs.AccountFor(s.cfg, &next).Validate(); err != nil {
			return err
		}
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE applications SET drm_enabled=?, drm_staging=?, drm_user=?, drm_password=?,
		       drm_merchant_uuid=?, drm_cpix_config_id=?, drm_asset_id=?
		WHERE id=?`,
		boolToInt(next.DRMEnabled), boolToInt(next.DRMStaging), next.DRMUser, next.DRMPassword,
		next.DRMMerchantUUID, next.DRMCPIXConfigID, next.DRMAssetID, id)
	if err != nil {
		return err
	}
	streamoutputs.ForgetDRM(s.cfg, old)
	return nil
}

func strPtrVal(p *string) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(*p)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// handleTestAppDRM fetches keys from DRMtoday with an application's stored
// settings and reports what came back (KIDs, their use, DRM systems) or
// DRMtoday's error. Body (optional): {"asset_id": "..."}; default is the
// app's template for a stream named "test". Keys themselves are never
// returned.
//
// POST /api/v1/applications/{id}/drm/test
func (s *Server) handleTestAppDRM(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req struct {
		AssetID string `json:"asset_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	var name string
	if err := s.db.QueryRowContext(r.Context(), `SELECT name FROM applications WHERE id = ?`, id).Scan(&name); err != nil {
		jsonError(w, http.StatusNotFound, "application not found")
		return
	}
	app, err := s.db.GetApplicationByName(r.Context(), name)
	if err != nil || app == nil {
		jsonError(w, http.StatusNotFound, "application not found")
		return
	}
	assetID := strings.TrimSpace(req.AssetID)
	if assetID == "" {
		assetID = drm.AssetID(app.DRMAssetID, app.Name, "test", "")
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	ks, err := streamoutputs.TestDRM(ctx, s.cfg, app, assetID)
	if err != nil {
		data, _ := json.Marshal(map[string]any{"ok": false, "asset_id": assetID, "error": err.Error()})
		jsonOK(w, data)
		return
	}
	type keyInfo struct {
		KID     string   `json:"kid"`
		Use     string   `json:"use"`
		Systems []string `json:"systems"`
	}
	audio := ks.AudioKey().KID
	var keys []keyInfo
	for _, k := range ks.Keys {
		use := "video"
		if k.KID == audio {
			use = "audio"
		}
		var systems []string
		for _, sys := range ks.SystemsFor(k.KID) {
			n := drm.SystemName(sys.SystemID)
			if n == "" {
				n = sys.SystemID
			}
			systems = append(systems, n)
		}
		keys = append(keys, keyInfo{KID: k.KIDString(), Use: use, Systems: systems})
	}
	data, _ := json.Marshal(map[string]any{"ok": true, "asset_id": assetID, "keys": keys})
	jsonOK(w, data)
}

// streamConfigDRM is the DRM part of a stream config write.
type streamConfigDRM struct {
	DRMEnabled *int    `json:"drm_enabled"`  // -1 inherit, 0 off, 1 on
	DRMAssetID *string `json:"drm_asset_id"` // "" = inherit
}

func (s *Server) applyStreamConfigDRM(ctx context.Context, sid string, d streamConfigDRM) error {
	if d.DRMEnabled != nil {
		if v := *d.DRMEnabled; v < -1 || v > 1 {
			return errors.New("drm_enabled must be -1, 0 or 1")
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE stream_configs SET drm_enabled=? WHERE id=?`, *d.DRMEnabled, sid); err != nil {
			return err
		}
	}
	if d.DRMAssetID != nil {
		if _, err := s.db.ExecContext(ctx, `UPDATE stream_configs SET drm_asset_id=? WHERE id=?`, strings.TrimSpace(*d.DRMAssetID), sid); err != nil {
			return err
		}
	}
	return nil
}
