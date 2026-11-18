package streamoutputs

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/config"
	"github.com/5centscdn/origin-mediaserver/internal/db"
	"github.com/5centscdn/origin-mediaserver/internal/drm"
	"github.com/5centscdn/origin-mediaserver/internal/dvr"
	"github.com/5centscdn/origin-mediaserver/internal/media"
)

// DRM (castLabs DRMtoday, internal/drm) per stream:
//
//   - An application (or a stream config overriding it) with DRM enabled
//     marks each of its streams Protected (media.DRMState) before any
//     output starts. Protected streams are served only encrypted: HLS
//     (forced to fMP4), LL-HLS, DASH and nDVR. Clear outputs (MPEG-TS over
//     HTTP, RTMP/SRT playback, WHEP) check ClearPlaybackAllowed and refuse.
//   - Keys are fetched once per asset (drm.Provider caches them) before the
//     packagers start; until they arrive no HLS/DASH/DVR output exists at
//     all, and a failure is retried with backoff and shown on the stream
//     (DRMState.Error). Clear output is never a fallback.
//   - The asset ID template ({app}_{group} by default) gives all %i
//     renditions of an ABR group one asset; DRMtoday's usage rules then pick
//     the video key per resolution.

var drmState struct {
	mu       sync.RWMutex
	cfg      *config.Config
	database *db.DB
	provider *drm.Provider
}

// ConfigureDRM sets up DRM for every stream; call once at startup, before
// any stream publishes. It also gives the nDVR engine its key lookup for
// encrypted recordings.
func ConfigureDRM(cfg *config.Config, database *db.DB, dvrEngine *dvr.Engine) {
	timeout := time.Duration(cfg.DRM.RequestTimeoutS) * time.Second
	p := drm.NewProvider(drm.NewClient(timeout))
	drmState.mu.Lock()
	drmState.cfg, drmState.database, drmState.provider = cfg, database, p
	drmState.mu.Unlock()
	if dvrEngine != nil {
		dvrEngine.SetKeyLookup(func(ctx context.Context, app, stream, assetID string) (*drm.KeySet, error) {
			a, err := database.GetApplicationByName(ctx, app)
			if err != nil {
				return nil, err
			}
			return p.Keys(ctx, AccountFor(cfg, a), assetID)
		})
	}
}

func drmDeps() (*config.Config, *db.DB, *drm.Provider) {
	drmState.mu.RLock()
	defer drmState.mu.RUnlock()
	return drmState.cfg, drmState.database, drmState.provider
}

// AccountFor builds an application's DRMtoday account (nil app → empty
// account, which fails validation).
func AccountFor(cfg *config.Config, a *db.AppConfig) drm.Account {
	var acc drm.Account
	if a != nil {
		acc = drm.Account{
			Staging:      a.DRMStaging,
			User:         a.DRMUser,
			Password:     a.DRMPassword,
			MerchantUUID: a.DRMMerchantUUID,
			CPIXConfigID: a.DRMCPIXConfigID,
		}
	}
	if cfg != nil {
		acc.AuthURL, acc.FEURL = cfg.DRM.DRMtodayAuthURL, cfg.DRM.DRMtodayFEURL
	}
	return acc
}

// TestDRM fetches an asset's keys with an application's settings, for the
// panel's "Test connection" (bypasses the cache).
func TestDRM(ctx context.Context, cfg *config.Config, a *db.AppConfig, assetID string) (*drm.KeySet, error) {
	timeout := time.Duration(cfg.DRM.RequestTimeoutS) * time.Second
	return drm.NewClient(timeout).Keys(ctx, AccountFor(cfg, a), assetID)
}

// ForgetDRM drops cached keys after an application's DRM settings change.
func ForgetDRM(cfg *config.Config, a *db.AppConfig) {
	if _, _, p := drmDeps(); p != nil && a != nil {
		p.Forget(AccountFor(cfg, a))
	}
}

// resolveDRM reports whether stream is DRM-protected and with which asset.
func resolveDRM(appCfg *db.AppConfig, streamCfg *db.StreamConfig, stream *media.Stream) (on bool, assetID string) {
	if appCfg == nil {
		return false, ""
	}
	on = appCfg.DRMEnabled
	tmpl := appCfg.DRMAssetID
	if streamCfg != nil {
		on = db.EffectiveOut(streamCfg.DRMEnabled, on)
		if streamCfg.DRMAssetID != "" {
			tmpl = streamCfg.DRMAssetID
		}
	}
	return on, drm.AssetID(tmpl, stream.Key.App, stream.Key.Name, stream.ABRGroup)
}

// ClearPlaybackAllowed reports whether stream may be served unencrypted
// (MPEG-TS over HTTP, RTMP/SRT playback, WHEP). It checks the stream's
// state and, so a player can't slip in between publish and outputs
// starting, the configuration itself.
func ClearPlaybackAllowed(ctx context.Context, stream *media.Stream) bool {
	if stream == nil {
		return true
	}
	if stream.DRM().Protected {
		return false
	}
	_, database, _ := drmDeps()
	if database == nil {
		return true
	}
	a, err := database.GetApplicationByName(ctx, stream.Key.App)
	if err != nil {
		return false // can't tell: fail closed
	}
	if a == nil {
		return true
	}
	sc, _ := database.GetStreamConfig(ctx, a.ID, stream.Key.Name)
	on, _ := resolveDRM(a, sc, stream)
	return !on
}

// startWithKeys fetches the stream's keys (retrying with backoff until ctx
// ends) and then calls start with them.
func startWithKeys(ctx context.Context, appCfg *db.AppConfig, stream *media.Stream, assetID string, start func(*drm.KeySet)) {
	cfg, _, provider := drmDeps()
	if provider == nil {
		slog.Error("DRM enabled but not configured; outputs stay off", "stream", stream.Key)
		stream.SetDRM(media.DRMState{Protected: true, AssetID: assetID, Error: "DRM not configured on this server"})
		return
	}
	acc := AccountFor(cfg, appCfg)
	go func() {
		backoff := 2 * time.Second
		for {
			ks, err := provider.Keys(ctx, acc, assetID)
			if err == nil {
				stream.SetDRM(media.DRMState{
					Protected:      true,
					AssetID:        assetID,
					HLSMasterLines: ks.HLSMasterLines(),
					Keys:           ks,
				})
				slog.Info("DRM keys ready", "stream", stream.Key, "asset", assetID, "keys", len(ks.Keys))
				start(ks)
				return
			}
			if ctx.Err() != nil {
				return
			}
			slog.Error("DRM key fetch failed; outputs stay off", "stream", stream.Key, "asset", assetID, "err", err, "retry_in", backoff)
			stream.SetDRM(media.DRMState{Protected: true, AssetID: assetID, Error: err.Error()})
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, time.Minute)
		}
	}()
}
