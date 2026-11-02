package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

type ctxKey string

const ctxUserID ctxKey = "user_id"
const ctxUserRole ctxKey = "user_role"

// ─── Bootstrap ───────────────────────────────────────────────────────────────

// EnsureBootstrapAdmin creates the initial admin user on first run if no users exist.
func (s *Server) EnsureBootstrapAdmin(ctx context.Context) error {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}

	cfg := s.cfg.Auth.BootstrapAdmin
	if cfg.Email == "" {
		return nil
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(cfg.Password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO users (id, email, password_hash, role) VALUES (?, ?, ?, ?)`,
		uuid.New().String(), cfg.Email, string(hash), "admin",
	)
	return err
}

// ─── Login ────────────────────────────────────────────────────────────────────

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type loginResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Role         string `json:"role"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	var userID, passwordHash, role string
	err := s.db.QueryRowContext(r.Context(),
		`SELECT id, password_hash, role FROM users WHERE email = ?`, req.Email,
	).Scan(&userID, &passwordHash, &role)
	if err != nil {
		jsonError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(req.Password)); err != nil {
		jsonError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}

	accessToken, err := s.generateJWT(userID, role, time.Duration(s.cfg.Auth.JWTExpiryHours)*time.Hour)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "token error")
		return
	}

	refreshToken, err := s.generateJWT(userID, role, time.Duration(s.cfg.Auth.RefreshExpiryDays)*24*time.Hour)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "token error")
		return
	}

	resp := loginResponse{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresIn:    s.cfg.Auth.JWTExpiryHours * 3600,
		Role:         role,
	}

	data, _ := json.Marshal(resp)
	jsonOK(w, data)
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	claims, err := s.validateJWT(body.RefreshToken)
	if err != nil {
		jsonError(w, http.StatusUnauthorized, "invalid token")
		return
	}

	userID := claims["sub"].(string)
	role := claims["role"].(string)

	accessToken, err := s.generateJWT(userID, role, time.Duration(s.cfg.Auth.JWTExpiryHours)*time.Hour)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "token error")
		return
	}

	data, _ := json.Marshal(map[string]string{"access_token": accessToken})
	jsonOK(w, data)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	// JWT is stateless — client discards the token.
	// In a full implementation, add the token to a short-lived revocation set in SQLite.
	w.WriteHeader(http.StatusNoContent)
}

// ─── JWT helpers ──────────────────────────────────────────────────────────────

func (s *Server) generateJWT(userID, role string, ttl time.Duration) (string, error) {
	claims := jwt.MapClaims{
		"sub":  userID,
		"role": role,
		"iat":  time.Now().Unix(),
		"exp":  time.Now().Add(ttl).Unix(),
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(s.cfg.Auth.JWTSecret))
}

func (s *Server) validateJWT(tokenStr string) (jwt.MapClaims, error) {
	token, err := jwt.Parse(tokenStr, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, jwt.ErrSignatureInvalid
		}
		return []byte(s.cfg.Auth.JWTSecret), nil
	})
	if err != nil || !token.Valid {
		return nil, err
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return nil, jwt.ErrTokenInvalidClaims
	}
	return claims, nil
}

// ─── Auth middleware ──────────────────────────────────────────────────────────

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			// Also check api_key query param for stream key auth.
			if apiKey := r.URL.Query().Get("api_key"); apiKey != "" {
				if userID, role, ok := s.validateAPIToken(r.Context(), apiKey); ok {
					ctx := context.WithValue(r.Context(), ctxUserID, userID)
					ctx = context.WithValue(ctx, ctxUserRole, role)
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
			}
			jsonError(w, http.StatusUnauthorized, "missing authorization")
			return
		}

		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
			jsonError(w, http.StatusUnauthorized, "invalid authorization header")
			return
		}

		claims, err := s.validateJWT(parts[1])
		if err != nil {
			jsonError(w, http.StatusUnauthorized, "invalid token")
			return
		}

		userID, _ := claims["sub"].(string)
		role, _ := claims["role"].(string)

		ctx := context.WithValue(r.Context(), ctxUserID, userID)
		ctx = context.WithValue(ctx, ctxUserRole, role)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) validateAPIToken(ctx context.Context, raw string) (userID, role string, ok bool) {
	// Scan all active tokens and compare using bcrypt (token_hash stores bcrypt hash).
	rows, err := s.db.QueryContext(ctx,
		`SELECT t.token_hash, u.id, u.role FROM api_tokens t JOIN users u ON u.id = t.user_id
		 WHERE (t.expires_at IS NULL OR t.expires_at > datetime('now'))`)
	if err != nil {
		return "", "", false
	}
	defer rows.Close()
	for rows.Next() {
		var hash, uid, r string
		if err := rows.Scan(&hash, &uid, &r); err != nil {
			continue
		}
		if bcrypt.CompareHashAndPassword([]byte(hash), []byte(raw)) == nil {
			return uid, r, true
		}
	}
	return "", "", false
}

func (s *Server) requireRole(role string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			userRole, _ := r.Context().Value(ctxUserRole).(string)
			if !hasRole(userRole, role) {
				jsonError(w, http.StatusForbidden, "insufficient permissions")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// requireRoleByMethod is the default policy for management routes:
// read-only requests (GET/HEAD) need viewer, anything that changes state
// needs operator.
func (s *Server) requireRoleByMethod(next http.Handler) http.Handler {
	viewer, operator := s.requireRole("viewer")(next), s.requireRole("operator")(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			viewer.ServeHTTP(w, r)
			return
		}
		operator.ServeHTTP(w, r)
	})
}

// requireSelfOrRole allows a request whose {id} URL param is the caller's
// own user ID, or any caller with at least role.
func (s *Server) requireSelfOrRole(role string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			userID, _ := r.Context().Value(ctxUserID).(string)
			userRole, _ := r.Context().Value(ctxUserRole).(string)
			if (userID == "" || userID != chi.URLParam(r, "id")) && !hasRole(userRole, role) {
				jsonError(w, http.StatusForbidden, "insufficient permissions")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func hasRole(userRole, required string) bool {
	order := map[string]int{"viewer": 1, "operator": 2, "admin": 3}
	return order[userRole] >= order[required]
}

// ─── User management ─────────────────────────────────────────────────────────

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT id, email, role, created_at FROM users ORDER BY created_at`)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	var users []map[string]interface{}
	for rows.Next() {
		var id, email, role, createdAt string
		if err := rows.Scan(&id, &email, &role, &createdAt); err != nil {
			continue
		}
		users = append(users, map[string]interface{}{
			"id": id, "email": email, "role": role, "created_at": createdAt,
		})
	}
	if users == nil {
		users = []map[string]interface{}{}
	}
	data, _ := json.Marshal(users)
	jsonOK(w, data)
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.Email == "" || req.Password == "" {
		jsonError(w, http.StatusBadRequest, "email and password are required")
		return
	}
	if req.Role == "" {
		req.Role = "viewer"
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "failed to hash password")
		return
	}

	id := uuid.New().String()
	_, err = s.db.ExecContext(r.Context(),
		`INSERT INTO users (id, email, password_hash, role) VALUES (?, ?, ?, ?)`,
		id, req.Email, string(hash), req.Role,
	)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var createdAt string
	s.db.QueryRowContext(r.Context(),
		`SELECT created_at FROM users WHERE id = ?`, id,
	).Scan(&createdAt)

	data, _ := json.Marshal(map[string]interface{}{
		"id": id, "email": req.Email, "role": req.Role, "created_at": createdAt,
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	w.Write(data)
}

func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req struct {
		Email    string `json:"email"`
		Role     string `json:"role"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	if req.Password != "" {
		hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to hash password")
			return
		}
		res, err := s.db.ExecContext(r.Context(),
			`UPDATE users SET email=?, role=?, password_hash=?, updated_at=datetime('now') WHERE id=?`,
			req.Email, req.Role, string(hash), id,
		)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			jsonError(w, http.StatusNotFound, "user not found")
			return
		}
	} else {
		res, err := s.db.ExecContext(r.Context(),
			`UPDATE users SET email=?, role=?, updated_at=datetime('now') WHERE id=?`,
			req.Email, req.Role, id,
		)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			jsonError(w, http.StatusNotFound, "user not found")
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	_, err := s.db.ExecContext(r.Context(), `DELETE FROM users WHERE id=?`, id)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ─── API token management ─────────────────────────────────────────────────────

func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	userID := chi.URLParam(r, "id")
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT id, name, scopes, expires_at, created_at FROM api_tokens WHERE user_id = ? ORDER BY created_at`,
		userID)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	var tokens []map[string]interface{}
	for rows.Next() {
		var tid, name, scopesJSON, createdAt string
		var expiresAt *string
		if err := rows.Scan(&tid, &name, &scopesJSON, &expiresAt, &createdAt); err != nil {
			continue
		}
		var scopes []string
		if err := json.Unmarshal([]byte(scopesJSON), &scopes); err != nil {
			scopes = []string{}
		}
		tokens = append(tokens, map[string]interface{}{
			"id": tid, "name": name, "scopes": scopes,
			"expires_at": expiresAt, "created_at": createdAt,
		})
	}
	if tokens == nil {
		tokens = []map[string]interface{}{}
	}
	data, _ := json.Marshal(tokens)
	jsonOK(w, data)
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	userID := chi.URLParam(r, "id")
	var req struct {
		Name      string   `json:"name"`
		Scopes    []string `json:"scopes"`
		ExpiresAt *string  `json:"expires_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.Name == "" {
		jsonError(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.Scopes == nil {
		req.Scopes = []string{}
	}
	// Stored as a JSON-array string in the scopes TEXT column — decoded
	// back into a real array by handleListTokens, and echoed back below as
	// req.Scopes (already the real array the client sent), never this
	// encoded form.
	scopesJSON, err := json.Marshal(req.Scopes)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "failed to encode scopes")
		return
	}

	// Generate a random 32-byte token, hex-encoded.
	rawBytes := make([]byte, 32)
	if _, err := rand.Read(rawBytes); err != nil {
		jsonError(w, http.StatusInternalServerError, "failed to generate token")
		return
	}
	rawToken := hex.EncodeToString(rawBytes)

	hash, err := bcrypt.GenerateFromPassword([]byte(rawToken), bcrypt.DefaultCost)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "failed to hash token")
		return
	}

	tid := uuid.New().String()
	_, err = s.db.ExecContext(r.Context(),
		`INSERT INTO api_tokens (id, user_id, name, token_hash, scopes, expires_at) VALUES (?, ?, ?, ?, ?, ?)`,
		tid, userID, req.Name, string(hash), string(scopesJSON), req.ExpiresAt,
	)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var createdAt string
	s.db.QueryRowContext(r.Context(),
		`SELECT created_at FROM api_tokens WHERE id = ?`, tid,
	).Scan(&createdAt)

	// Return the raw token ONCE — it will not be retrievable again.
	data, _ := json.Marshal(map[string]interface{}{
		"id": tid, "name": req.Name, "scopes": req.Scopes,
		"expires_at": req.ExpiresAt, "created_at": createdAt,
		"token": rawToken,
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	w.Write(data)
}

func (s *Server) handleDeleteToken(w http.ResponseWriter, r *http.Request) {
	userID := chi.URLParam(r, "id")
	tid := chi.URLParam(r, "tid")
	_, err := s.db.ExecContext(r.Context(),
		`DELETE FROM api_tokens WHERE id=? AND user_id=?`, tid, userID)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
