package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// OCI 레지스트리 Pull/Push 프로토콜 시뮬레이션
//
// tart의 Registry.swift 구현을 Go로 재현한다.
// 핵심 흐름:
//   1) Push: POST /v2/{ns}/blobs/uploads/ → PUT (monolithic) → PUT /manifests/{ref}
//   2) Pull: GET /v2/{ns}/manifests/{ref} → GET /v2/{ns}/blobs/{digest}
//   3) 인증: 401 → WWW-Authenticate Bearer → 토큰 요청 → 재시도
//   4) SHA256 다이제스트 검증
//
// 참조: tart/Sources/tart/OCI/Registry.swift
//       tart/Sources/tart/OCI/Manifest.swift
//       tart/Sources/tart/OCI/Digest.swift
//       tart/Sources/tart/OCI/WWWAuthenticate.swift
//       tart/Sources/tart/OCI/Authentication.swift
//       tart/Sources/tart/OCI/AuthenticationKeeper.swift
// =============================================================================

// --- OCI 매니페스트 구조체 (tart Manifest.swift 참조) ---

// OCIManifestConfig는 매니페스트의 config 블록이다.
// tart의 OCIManifestConfig와 동일한 구조.
type OCIManifestConfig struct {
	MediaType string `json:"mediaType"`
	Size      int    `json:"size"`
	Digest    string `json:"digest"`
}

// OCIManifestLayer는 매니페스트의 개별 레이어를 나타낸다.
// tart의 OCIManifestLayer와 동일한 구조.
type OCIManifestLayer struct {
	MediaType   string            `json:"mediaType"`
	Size        int               `json:"size"`
	Digest      string            `json:"digest"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// OCIManifest는 OCI 이미지 매니페스트 전체이다.
// tart의 OCIManifest와 동일한 구조 (schemaVersion, mediaType, config, layers).
type OCIManifest struct {
	SchemaVersion int                `json:"schemaVersion"`
	MediaType     string             `json:"mediaType"`
	Config        OCIManifestConfig  `json:"config"`
	Layers        []OCIManifestLayer `json:"layers"`
	Annotations   map[string]string  `json:"annotations,omitempty"`
}

// --- SHA256 다이제스트 유틸리티 (tart Digest.swift 참조) ---

// computeDigest는 데이터의 SHA256 다이제스트를 "sha256:..." 형식으로 반환한다.
// tart의 Digest.hash(_ data: Data) -> String과 동일한 동작.
func computeDigest(data []byte) string {
	h := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%x", h)
}

// --- Bearer 토큰 응답 (tart TokenResponse 참조) ---

// TokenResponse는 토큰 서버가 반환하는 인증 응답이다.
// tart의 TokenResponse 구조체와 동일: token, expires_in, issued_at 포함.
type TokenResponse struct {
	Token     string `json:"token"`
	ExpiresIn int    `json:"expires_in"`
	IssuedAt  string `json:"issued_at"`
}

// --- OCI 레지스트리 서버 (tart Registry.swift의 서버 측 시뮬레이션) ---

// RegistryServer는 OCI Distribution Spec의 핵심 엔드포인트를 구현한다.
// tart의 Registry 클래스가 호출하는 서버 측 API를 시뮬레이션.
type RegistryServer struct {
	mu        sync.RWMutex
	manifests map[string][]byte            // namespace/reference → manifest JSON
	blobs     map[string][]byte            // sha256:... → blob data
	uploads   map[string][]byte            // upload UUID → 중간 데이터
	tokens    map[string]time.Time         // token → 만료시간
	users     map[string]string            // username → password (인증용)
	tokenURL  string                       // Bearer 토큰 엔드포인트 URL
}

// NewRegistryServer는 레지스트리 서버를 초기화한다.
func NewRegistryServer() *RegistryServer {
	return &RegistryServer{
		manifests: make(map[string][]byte),
		blobs:     make(map[string][]byte),
		uploads:   make(map[string][]byte),
		tokens:    make(map[string]time.Time),
		users:     map[string]string{"admin": "secret123"},
	}
}

// generateToken은 인증된 사용자에게 Bearer 토큰을 발급한다.
// tart의 TokenResponse.parse와 tokenExpiresAt 로직을 반영.
func (rs *RegistryServer) generateToken() string {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	// 간단한 토큰 생성 (실제로는 JWT 등 사용)
	token := fmt.Sprintf("token-%d", time.Now().UnixNano())
	// tart 기본값: expiresIn이 nil이면 60초 (Registry.swift TokenResponse.tokenExpiresAt)
	rs.tokens[token] = time.Now().Add(60 * time.Second)
	return token
}

// validateToken은 Bearer 토큰의 유효성을 검사한다.
// tart의 AuthenticationKeeper.header()에서 isValid() 체크하는 것과 동일.
func (rs *RegistryServer) validateToken(r *http.Request) bool {
	rs.mu.RLock()
	defer rs.mu.RUnlock()

	auth := r.Header.Get("Authorization")
	if auth == "" {
		return false
	}

	if strings.HasPrefix(auth, "Bearer ") {
		token := strings.TrimPrefix(auth, "Bearer ")
		expiry, ok := rs.tokens[token]
		if !ok {
			return false
		}
		return time.Now().Before(expiry)
	}
	return false
}

// ServeHTTP는 모든 HTTP 요청을 라우팅한다.
// tart Registry.swift의 endpointURL, channelRequest 등이 호출하는 엔드포인트들.
func (rs *RegistryServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// --- 토큰 엔드포인트 (인증 서버) ---
	// tart의 auth() 메서드가 realm URL로 GET 요청을 보내는 대상
	if path == "/token" {
		rs.handleToken(w, r)
		return
	}

	// --- /v2/ ping 엔드포인트 ---
	// tart의 ping() → GET /v2/ → 200 OK
	if path == "/v2/" {
		if !rs.validateToken(r) {
			// tart: 401 → WWW-Authenticate Bearer 헤더 반환
			// Registry.swift channelRequest에서 401 감지 → auth() 호출
			w.Header().Set("WWW-Authenticate",
				fmt.Sprintf(`Bearer realm="%s/token",service="registry.example.com",scope="repository:test:pull,push"`, rs.tokenURL))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}

	// 인증 검증 (모든 /v2/ 하위 엔드포인트)
	if !rs.validateToken(r) {
		w.Header().Set("WWW-Authenticate",
			fmt.Sprintf(`Bearer realm="%s/token",service="registry.example.com",scope="repository:test:pull,push"`, rs.tokenURL))
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	// --- Manifest 엔드포인트 ---
	// PUT /v2/{ns}/manifests/{ref}: tart의 pushManifest()
	// GET /v2/{ns}/manifests/{ref}: tart의 pullManifest()
	if strings.Contains(path, "/manifests/") {
		rs.handleManifest(w, r)
		return
	}

	// --- Blob Upload 엔드포인트 ---
	// POST /v2/{ns}/blobs/uploads/: tart의 pushBlob() 1단계 (업로드 시작)
	if strings.HasSuffix(path, "/blobs/uploads/") && r.Method == http.MethodPost {
		rs.handleBlobUploadInit(w, r)
		return
	}

	// PUT /v2/{ns}/blobs/uploads/{uuid}: tart의 pushBlob() 2단계 (monolithic upload)
	if strings.Contains(path, "/blobs/uploads/") && r.Method == http.MethodPut {
		rs.handleBlobUploadComplete(w, r)
		return
	}

	// --- Blob 엔드포인트 ---
	// HEAD /v2/{ns}/blobs/{digest}: tart의 blobExists()
	// GET /v2/{ns}/blobs/{digest}: tart의 pullBlob()
	if strings.Contains(path, "/blobs/sha256:") {
		rs.handleBlob(w, r)
		return
	}

	http.NotFound(w, r)
}

// handleToken은 Bearer 토큰 발급을 처리한다.
// tart의 auth() → realm URL로 GET 요청 → Basic 인증 헤더 포함
// → 성공 시 TokenResponse 반환
func (rs *RegistryServer) handleToken(w http.ResponseWriter, r *http.Request) {
	// Basic 인증 확인 (tart: auth() 에서 lookupCredentials → base64 인코딩)
	auth := r.Header.Get("Authorization")
	if auth == "" || !strings.HasPrefix(auth, "Basic ") {
		http.Error(w, "인증 필요", http.StatusUnauthorized)
		return
	}

	// Base64 디코딩 (tart: "\(user):\(password)".data(using: .utf8)?.base64EncodedString())
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(auth, "Basic "))
	if err != nil {
		http.Error(w, "잘못된 인증 형식", http.StatusBadRequest)
		return
	}

	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 {
		http.Error(w, "잘못된 인증 형식", http.StatusBadRequest)
		return
	}

	// 자격증명 확인
	rs.mu.RLock()
	expectedPassword, ok := rs.users[parts[0]]
	rs.mu.RUnlock()

	if !ok || expectedPassword != parts[1] {
		http.Error(w, "잘못된 자격증명", http.StatusUnauthorized)
		return
	}

	// 토큰 발급 (tart: TokenResponse.parse → token + expires_in + issued_at)
	token := rs.generateToken()
	resp := TokenResponse{
		Token:     token,
		ExpiresIn: 60,
		IssuedAt:  time.Now().UTC().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleManifest는 매니페스트 Push/Pull을 처리한다.
// tart의 pushManifest()와 pullManifest() 대응.
func (rs *RegistryServer) handleManifest(w http.ResponseWriter, r *http.Request) {
	// 경로에서 namespace/reference 추출
	parts := strings.Split(r.URL.Path, "/manifests/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	key := parts[0] + "/" + parts[1] // 전체 경로를 키로 사용

	switch r.Method {
	case http.MethodPut:
		// Push Manifest (tart: pushManifest → PUT → 201 Created)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "본문 읽기 실패", http.StatusBadRequest)
			return
		}

		rs.mu.Lock()
		rs.manifests[key] = body
		rs.mu.Unlock()

		// 다이제스트 반환 (tart: Digest.hash(manifestJSON))
		digest := computeDigest(body)
		w.Header().Set("Docker-Content-Digest", digest)
		w.WriteHeader(http.StatusCreated)

	case http.MethodGet:
		// Pull Manifest (tart: pullManifest → GET → 200 OK + JSON)
		rs.mu.RLock()
		data, ok := rs.manifests[key]
		rs.mu.RUnlock()

		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Write(data)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// handleBlobUploadInit은 Blob 업로드 시작을 처리한다.
// tart의 pushBlob() 1단계: POST /v2/{ns}/blobs/uploads/ → 202 Accepted + Location 헤더
func (rs *RegistryServer) handleBlobUploadInit(w http.ResponseWriter, r *http.Request) {
	// 업로드 UUID 생성
	uuid := fmt.Sprintf("upload-%d", time.Now().UnixNano())

	rs.mu.Lock()
	rs.uploads[uuid] = []byte{}
	rs.mu.Unlock()

	// Location 헤더 반환 (tart: uploadLocationFromResponse → Location 헤더 파싱)
	location := fmt.Sprintf("%s%s%s", r.URL.Path, uuid, "/")
	w.Header().Set("Location", location)
	w.WriteHeader(http.StatusAccepted)
}

// handleBlobUploadComplete는 Blob 업로드 완료(monolithic)를 처리한다.
// tart의 pushBlob() 2단계: PUT → digest 파라미터 포함 → 201 Created
func (rs *RegistryServer) handleBlobUploadComplete(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "본문 읽기 실패", http.StatusBadRequest)
		return
	}

	// digest 쿼리 파라미터 확인 (tart: parameters: ["digest": digest])
	expectedDigest := r.URL.Query().Get("digest")
	actualDigest := computeDigest(body)

	// SHA256 다이제스트 검증 (tart: Digest.hash 로 무결성 확인)
	if expectedDigest != "" && expectedDigest != actualDigest {
		http.Error(w, fmt.Sprintf("다이제스트 불일치: 기대=%s, 실제=%s", expectedDigest, actualDigest), http.StatusBadRequest)
		return
	}

	rs.mu.Lock()
	rs.blobs[actualDigest] = body
	rs.mu.Unlock()

	w.Header().Set("Docker-Content-Digest", actualDigest)
	w.WriteHeader(http.StatusCreated)
}

// handleBlob는 Blob 조회(HEAD/GET)를 처리한다.
// tart의 blobExists() (HEAD) 및 pullBlob() (GET).
func (rs *RegistryServer) handleBlob(w http.ResponseWriter, r *http.Request) {
	// 경로에서 다이제스트 추출
	parts := strings.Split(r.URL.Path, "/blobs/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	digest := parts[1]

	rs.mu.RLock()
	data, ok := rs.blobs[digest]
	rs.mu.RUnlock()

	switch r.Method {
	case http.MethodHead:
		// Blob 존재 확인 (tart: blobExists → HEAD → 200 or 404)
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
		w.WriteHeader(http.StatusOK)

	case http.MethodGet:
		// Blob 다운로드 (tart: pullBlob → GET → 200 OK + 데이터)
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(data)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// =============================================================================
// OCI 레지스트리 클라이언트 (tart Registry 클래스 시뮬레이션)
// =============================================================================

// RegistryClient는 tart의 Registry 클래스를 Go로 재현한 것이다.
// 핵심 메서드: ping, pushBlob, pushManifest, pullManifest, pullBlob
// 인증 흐름: 401 → WWW-Authenticate 파싱 → 토큰 요청 → 재시도
type RegistryClient struct {
	baseURL   string
	namespace string
	username  string
	password  string
	token     string           // 현재 Bearer 토큰
	tokenExp  time.Time        // 토큰 만료시간
	client    *http.Client
}

// NewRegistryClient는 클라이언트를 생성한다.
// tart의 Registry.init(host:namespace:insecure:credentialsProviders:) 대응.
func NewRegistryClient(baseURL, namespace, username, password string) *RegistryClient {
	return &RegistryClient{
		baseURL:   baseURL,
		namespace: namespace,
		username:  username,
		password:  password,
		client:    &http.Client{},
	}
}

// doRequest는 인증 인식 HTTP 요청을 수행한다.
// tart의 channelRequest → authAwareRequest → 401이면 auth() → 재시도 흐름.
func (rc *RegistryClient) doRequest(method, url string, body io.Reader, headers map[string]string) (*http.Response, []byte, error) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, nil, fmt.Errorf("요청 생성 실패: %w", err)
	}

	// 헤더 설정
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	// tart의 authAwareRequest: 유효한 토큰이 있으면 Authorization 헤더 추가
	if rc.token != "" && time.Now().Before(rc.tokenExp) {
		req.Header.Set("Authorization", "Bearer "+rc.token)
	}

	resp, err := rc.client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("요청 실행 실패: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	// tart: channelRequest에서 401이면 auth(response:) 호출 후 재시도
	if resp.StatusCode == http.StatusUnauthorized {
		fmt.Println("  [클라이언트] 401 수신 → WWW-Authenticate 헤더 파싱 → 토큰 요청")
		err = rc.authenticate(resp)
		if err != nil {
			return resp, respBody, fmt.Errorf("인증 실패: %w", err)
		}
		// 재시도 (tart: auth 후 authAwareRequest 재호출)
		return rc.doRequestWithAuth(method, url, body, headers)
	}

	return resp, respBody, nil
}

// authenticate는 401 응답에서 WWW-Authenticate 헤더를 파싱하고 토큰을 요청한다.
// tart의 auth(response:) 메서드를 재현:
//   1) WWW-Authenticate 헤더 파싱 (WWWAuthenticate.swift)
//   2) realm URL 추출
//   3) scope, service 쿼리 파라미터 추가
//   4) Basic 인증으로 토큰 요청
//   5) TokenResponse 파싱 → token + expires_in
func (rc *RegistryClient) authenticate(resp *http.Response) error {
	// tart: response.value(forHTTPHeaderField: "WWW-Authenticate")
	wwwAuth := resp.Header.Get("WWW-Authenticate")
	if wwwAuth == "" {
		return fmt.Errorf("WWW-Authenticate 헤더 없음")
	}

	// tart: WWWAuthenticate(rawHeaderValue:) 파서로 scheme + kvs 추출
	// "Bearer realm=...,service=...,scope=..." 파싱
	scheme, kvs := parseWWWAuthenticate(wwwAuth)
	fmt.Printf("  [인증] 스킴: %s, realm: %s\n", scheme, kvs["realm"])

	if strings.ToLower(scheme) != "bearer" {
		return fmt.Errorf("지원하지 않는 인증 스킴: %s", scheme)
	}

	realm, ok := kvs["realm"]
	if !ok {
		return fmt.Errorf("realm 디렉티브 없음")
	}

	// tart: authenticateURL.queryItems = ["scope", "service"].compactMap { ... }
	tokenURL := realm
	params := []string{}
	for _, key := range []string{"scope", "service"} {
		if v, ok := kvs[key]; ok {
			params = append(params, fmt.Sprintf("%s=%s", key, v))
		}
	}
	if len(params) > 0 {
		tokenURL += "?" + strings.Join(params, "&")
	}

	// tart: Basic 인증 헤더 추가 → "\(user):\(password)".base64
	req, _ := http.NewRequest("GET", tokenURL, nil)
	creds := base64.StdEncoding.EncodeToString([]byte(rc.username + ":" + rc.password))
	req.Header.Set("Authorization", "Basic "+creds)

	tokenResp, err := rc.client.Do(req)
	if err != nil {
		return fmt.Errorf("토큰 요청 실패: %w", err)
	}
	defer tokenResp.Body.Close()

	if tokenResp.StatusCode != http.StatusOK {
		return fmt.Errorf("토큰 요청 HTTP %d", tokenResp.StatusCode)
	}

	// tart: TokenResponse.parse(fromData:)
	var tr TokenResponse
	if err := json.NewDecoder(tokenResp.Body).Decode(&tr); err != nil {
		return fmt.Errorf("토큰 응답 파싱 실패: %w", err)
	}

	rc.token = tr.Token
	// tart: (issuedAt ?? Date()) + TimeInterval(expiresIn ?? 60)
	rc.tokenExp = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	fmt.Printf("  [인증] 토큰 획득 완료 (만료: %ds)\n", tr.ExpiresIn)

	return nil
}

// doRequestWithAuth는 인증 토큰을 포함하여 요청을 재시도한다.
func (rc *RegistryClient) doRequestWithAuth(method, url string, body io.Reader, headers map[string]string) (*http.Response, []byte, error) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Authorization", "Bearer "+rc.token)

	resp, err := rc.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	return resp, respBody, nil
}

// parseWWWAuthenticate는 WWW-Authenticate 헤더를 파싱한다.
// tart의 WWWAuthenticate.swift 구현을 재현:
//   - 공백으로 scheme과 디렉티브 분리
//   - 쉼표로 key=value 쌍 분리 (따옴표 내 쉼표는 무시)
//   - 따옴표 제거
func parseWWWAuthenticate(raw string) (string, map[string]string) {
	kvs := make(map[string]string)

	// tart: rawHeaderValue.split(separator: " ", maxSplits: 1)
	idx := strings.Index(raw, " ")
	if idx == -1 {
		return raw, kvs
	}
	scheme := raw[:idx]
	rest := raw[idx+1:]

	// tart: contextAwareCommaSplit — 따옴표 안의 쉼표는 무시
	var parts []string
	inQuote := false
	current := strings.Builder{}
	for _, ch := range rest {
		if ch == ',' && !inQuote {
			parts = append(parts, current.String())
			current.Reset()
			continue
		}
		current.WriteRune(ch)
		if ch == '"' {
			inQuote = !inQuote
		}
	}
	if current.Len() > 0 {
		parts = append(parts, current.String())
	}

	// tart: parts.split(separator: "=", maxSplits: 1) → key, value
	for _, part := range parts {
		eqIdx := strings.Index(part, "=")
		if eqIdx == -1 {
			continue
		}
		key := strings.TrimSpace(part[:eqIdx])
		value := strings.Trim(strings.TrimSpace(part[eqIdx+1:]), `"`)
		kvs[key] = value
	}

	return scheme, kvs
}

// --- Push/Pull 고수준 메서드 (tart Registry 클래스 메서드 대응) ---

// PushBlob는 blob 데이터를 레지스트리에 Push한다.
// tart의 pushBlob(fromData:chunkSizeMb:digest:) 흐름:
//   1) POST /v2/{ns}/blobs/uploads/ → 202 + Location
//   2) PUT {location}?digest={sha256} → 201 Created (monolithic upload)
func (rc *RegistryClient) PushBlob(data []byte) (string, error) {
	digest := computeDigest(data)

	// 1단계: 업로드 시작 (tart: POST → 202 Accepted)
	initURL := fmt.Sprintf("%s/v2/%s/blobs/uploads/", rc.baseURL, rc.namespace)
	resp, _, err := rc.doRequest("POST", initURL, nil, map[string]string{"Content-Length": "0"})
	if err != nil {
		return "", fmt.Errorf("blob 업로드 시작 실패: %w", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		return "", fmt.Errorf("blob 업로드 시작 HTTP %d", resp.StatusCode)
	}

	// Location 헤더에서 업로드 URL 추출 (tart: uploadLocationFromResponse)
	location := resp.Header.Get("Location")
	if location == "" {
		return "", fmt.Errorf("Location 헤더 없음")
	}

	// 2단계: monolithic upload (tart: PUT + digest 파라미터)
	uploadURL := fmt.Sprintf("%s%s?digest=%s", rc.baseURL, location, digest)
	resp2, _, err := rc.doRequest("PUT", uploadURL, strings.NewReader(string(data)),
		map[string]string{"Content-Type": "application/octet-stream"})
	if err != nil {
		return "", fmt.Errorf("blob 업로드 완료 실패: %w", err)
	}
	if resp2.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("blob 업로드 완료 HTTP %d", resp2.StatusCode)
	}

	return digest, nil
}

// PushManifest는 매니페스트를 레지스트리에 Push한다.
// tart의 pushManifest(reference:manifest:) 대응:
//   PUT /v2/{ns}/manifests/{ref} → 201 Created
func (rc *RegistryClient) PushManifest(reference string, manifest OCIManifest) (string, error) {
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return "", fmt.Errorf("매니페스트 직렬화 실패: %w", err)
	}

	url := fmt.Sprintf("%s/v2/%s/manifests/%s", rc.baseURL, rc.namespace, reference)
	resp, _, err := rc.doRequest("PUT", url, strings.NewReader(string(manifestJSON)),
		map[string]string{"Content-Type": "application/vnd.oci.image.manifest.v1+json"})
	if err != nil {
		return "", fmt.Errorf("매니페스트 Push 실패: %w", err)
	}
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("매니페스트 Push HTTP %d", resp.StatusCode)
	}

	return computeDigest(manifestJSON), nil
}

// PullManifest는 레지스트리에서 매니페스트를 Pull한다.
// tart의 pullManifest(reference:) 대응:
//   GET /v2/{ns}/manifests/{ref} → 200 OK + JSON
func (rc *RegistryClient) PullManifest(reference string) (*OCIManifest, error) {
	url := fmt.Sprintf("%s/v2/%s/manifests/%s", rc.baseURL, rc.namespace, reference)
	resp, body, err := rc.doRequest("GET", url, nil,
		map[string]string{"Accept": "application/vnd.oci.image.manifest.v1+json"})
	if err != nil {
		return nil, fmt.Errorf("매니페스트 Pull 실패: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("매니페스트 Pull HTTP %d", resp.StatusCode)
	}

	var manifest OCIManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return nil, fmt.Errorf("매니페스트 파싱 실패: %w", err)
	}

	return &manifest, nil
}

// PullBlob는 레지스트리에서 blob을 Pull한다.
// tart의 pullBlob(_:rangeStart:handler:) 대응:
//   GET /v2/{ns}/blobs/{digest} → 200 OK + 데이터
func (rc *RegistryClient) PullBlob(digest string) ([]byte, error) {
	url := fmt.Sprintf("%s/v2/%s/blobs/%s", rc.baseURL, rc.namespace, digest)
	resp, body, err := rc.doRequest("GET", url, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("blob Pull 실패: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("blob Pull HTTP %d", resp.StatusCode)
	}

	return body, nil
}

// =============================================================================
// 메인 함수: 전체 OCI Push/Pull 워크플로우 시연
// =============================================================================

func main() {
	fmt.Println("=== OCI 레지스트리 Pull/Push 프로토콜 시뮬레이션 ===")
	fmt.Println("(tart Registry.swift, Manifest.swift, Digest.swift 기반)")
	fmt.Println()

	// --- 1. 레지스트리 서버 시작 ---
	registry := NewRegistryServer()
	server := httptest.NewServer(registry)
	defer server.Close()
	registry.tokenURL = server.URL // 토큰 엔드포인트 URL 설정
	fmt.Printf("[서버] OCI 레지스트리 서버 시작: %s\n\n", server.URL)

	// --- 2. 클라이언트 생성 ---
	client := NewRegistryClient(server.URL, "library/tart-vm", "admin", "secret123")

	// --- 3. Push 워크플로우 ---
	fmt.Println("========== PUSH 워크플로우 ==========")
	fmt.Println()

	// 3-1. Config blob Push
	fmt.Println("[Push 1/3] Config blob 업로드")
	configData := []byte(`{"architecture":"arm64","os":"darwin"}`)
	configDigest, err := client.PushBlob(configData)
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
		return
	}
	fmt.Printf("  Config 다이제스트: %s\n\n", configDigest)

	// 3-2. Disk layer blob Push
	fmt.Println("[Push 2/3] Disk layer blob 업로드")
	diskData := []byte("이것은 macOS VM 디스크 이미지의 압축된 레이어 데이터입니다. " +
		"실제로는 LZ4로 압축된 수백 MB의 디스크 데이터가 들어갑니다.")
	diskDigest, err := client.PushBlob(diskData)
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
		return
	}
	fmt.Printf("  Disk 레이어 다이제스트: %s\n\n", diskDigest)

	// 3-3. NVRAM layer blob Push
	fmt.Println("[Push 3/3] NVRAM blob 업로드")
	nvramData := []byte("NVRAM-DATA-FOR-VM-BOOT-CONFIGURATION")
	nvramDigest, err := client.PushBlob(nvramData)
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
		return
	}
	fmt.Printf("  NVRAM 다이제스트: %s\n\n", nvramDigest)

	// 3-4. Manifest Push
	fmt.Println("[Push 4/4] Manifest 업로드")
	manifest := OCIManifest{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.manifest.v1+json",
		Config: OCIManifestConfig{
			MediaType: "application/vnd.cirruslabs.tart.config.v1",
			Size:      len(configData),
			Digest:    configDigest,
		},
		Layers: []OCIManifestLayer{
			{
				MediaType: "application/vnd.cirruslabs.tart.disk.v2",
				Size:      len(diskData),
				Digest:    diskDigest,
				Annotations: map[string]string{
					"org.cirruslabs.tart.uncompressed-size":           "1073741824",
					"org.cirruslabs.tart.uncompressed-content-digest": "sha256:uncompressed-disk-hash",
				},
			},
			{
				MediaType: "application/vnd.cirruslabs.tart.nvram.v1",
				Size:      len(nvramData),
				Digest:    nvramDigest,
			},
		},
		Annotations: map[string]string{
			"org.cirruslabs.tart.uncompressed-disk-size": "1073741824",
		},
	}

	manifestDigest, err := client.PushManifest("latest", manifest)
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
		return
	}
	fmt.Printf("  Manifest 다이제스트: %s\n\n", manifestDigest)

	// --- 4. Pull 워크플로우 ---
	fmt.Println("========== PULL 워크플로우 ==========")
	fmt.Println()

	// 4-1. Manifest Pull
	fmt.Println("[Pull 1/4] Manifest 다운로드")
	pulledManifest, err := client.PullManifest("latest")
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
		return
	}
	fmt.Printf("  스키마 버전: %d\n", pulledManifest.SchemaVersion)
	fmt.Printf("  Config 다이제스트: %s\n", pulledManifest.Config.Digest)
	fmt.Printf("  레이어 수: %d\n\n", len(pulledManifest.Layers))

	// 4-2. Config blob Pull
	fmt.Println("[Pull 2/4] Config blob 다운로드")
	pulledConfig, err := client.PullBlob(pulledManifest.Config.Digest)
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
		return
	}
	// 다이제스트 검증 (tart: Digest.hash로 무결성 확인)
	verifyDigest := computeDigest(pulledConfig)
	fmt.Printf("  Config 내용: %s\n", string(pulledConfig))
	fmt.Printf("  다이제스트 검증: %s == %s → %v\n\n",
		verifyDigest[:20]+"...", pulledManifest.Config.Digest[:20]+"...",
		verifyDigest == pulledManifest.Config.Digest)

	// 4-3. 각 레이어 Pull
	for i, layer := range pulledManifest.Layers {
		fmt.Printf("[Pull %d/4] 레이어 %d 다운로드 (%s)\n", i+3, i+1, layer.MediaType)
		pulledLayer, err := client.PullBlob(layer.Digest)
		if err != nil {
			fmt.Printf("  오류: %v\n", err)
			return
		}
		layerDigest := computeDigest(pulledLayer)
		fmt.Printf("  크기: %d 바이트\n", len(pulledLayer))
		fmt.Printf("  다이제스트 검증: %v\n", layerDigest == layer.Digest)
		if uncompSize, ok := layer.Annotations["org.cirruslabs.tart.uncompressed-size"]; ok {
			fmt.Printf("  비압축 크기: %s 바이트\n", uncompSize)
		}
		fmt.Println()
	}

	// --- 5. 인증 만료 시뮬레이션 ---
	fmt.Println("========== 인증 만료 시뮬레이션 ==========")
	fmt.Println()

	// 토큰을 강제 만료시켜 재인증 흐름 테스트
	client.tokenExp = time.Now().Add(-1 * time.Hour) // 만료된 토큰
	fmt.Println("[테스트] 만료된 토큰으로 Manifest Pull 시도")
	pulledManifest2, err := client.PullManifest("latest")
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
		return
	}
	fmt.Printf("  재인증 후 Manifest Pull 성공! 레이어 수: %d\n\n", len(pulledManifest2.Layers))

	// --- 6. 요약 ---
	fmt.Println("========== 요약 ==========")
	fmt.Println()
	fmt.Println("시뮬레이션된 OCI Distribution Spec 엔드포인트:")
	fmt.Println("  POST   /v2/{ns}/blobs/uploads/        → Blob 업로드 시작 (202)")
	fmt.Println("  PUT    /v2/{ns}/blobs/uploads/{uuid}   → Blob 업로드 완료 (201)")
	fmt.Println("  HEAD   /v2/{ns}/blobs/{digest}         → Blob 존재 확인")
	fmt.Println("  GET    /v2/{ns}/blobs/{digest}         → Blob Pull")
	fmt.Println("  PUT    /v2/{ns}/manifests/{ref}        → Manifest Push (201)")
	fmt.Println("  GET    /v2/{ns}/manifests/{ref}        → Manifest Pull")
	fmt.Println("  GET    /token                          → Bearer 토큰 발급")
	fmt.Println()
	fmt.Println("시뮬레이션된 인증 흐름:")
	fmt.Println("  1) 클라이언트 → 서버: 인증 없이 요청")
	fmt.Println("  2) 서버 → 클라이언트: 401 + WWW-Authenticate Bearer realm=...")
	fmt.Println("  3) 클라이언트 → 토큰서버: GET /token + Basic 인증")
	fmt.Println("  4) 토큰서버 → 클라이언트: {token, expires_in, issued_at}")
	fmt.Println("  5) 클라이언트 → 서버: Bearer 토큰으로 재시도")
	fmt.Println()
	fmt.Println("[완료] OCI 레지스트리 프로토콜 시뮬레이션 성공")
}
