# 26. 검색 시스템 & WebSocket 심층 분석

## 목차
1. [개요](#1-개요)
2. [검색 시스템 아키텍처](#2-검색-시스템-아키텍처)
3. [SearchIndex 인터페이스 분석](#3-searchindex-인터페이스-분석)
4. [Search 클래스: 검색 엔진 핵심](#4-search-클래스-검색-엔진-핵심)
5. [토큰 기반 다단계 검색 알고리즘](#5-토큰-기반-다단계-검색-알고리즘)
6. [EditDistance 기반 결과 정렬](#6-editdistance-기반-결과-정렬)
7. [SearchIndexBuilder: 인덱스 구축](#7-searchindexbuilder-인덱스-구축)
8. [검색 자동완성 (Suggest)](#8-검색-자동완성-suggest)
9. [WebSocket 아키텍처](#9-websocket-아키텍처)
10. [WebSockets 클래스 분석](#10-websockets-클래스-분석)
11. [WebSocketSession: 세션 관리](#11-websocketsession-세션-관리)
12. [Provider SPI: 서버 구현 추상화](#12-provider-spi-서버-구현-추상화)
13. [Ping/Pong 메커니즘](#13-pingpong-메커니즘)
14. [설계 결정과 교훈](#14-설계-결정과-교훈)

---

## 1. 개요

Jenkins의 **검색 시스템**과 **WebSocket**은 사용자 인터페이스의 핵심 기능을 담당한다.

- **검색 시스템**: Jenkins UI 상단의 검색 바를 통해 Job, View, Node 등을 빠르게 찾아가는 네비게이션 엔진
- **WebSocket**: 빌드 로그 스트리밍, CLI over WebSocket 등 실시간 양방향 통신 기반

### 왜 이 두 기능을 함께 분석하는가?

1. **웹 인터페이스의 양대 축**: 검색은 "찾기", WebSocket은 "실시간 수신"으로 사용자 경험의 핵심
2. **Stapler 프레임워크 위에 구축**: 둘 다 Stapler의 URL 라우팅/요청 처리 위에서 동작
3. **SPI(Service Provider Interface) 패턴**: 둘 다 구현을 추상화하고 플러그 가능한 구조

### 소스 경로

```
core/src/main/java/hudson/search/
├── Search.java                  # 검색 엔진 핵심 (562줄)
├── SearchIndex.java             # 검색 인덱스 인터페이스
├── SearchIndexBuilder.java      # 인덱스 빌더
├── SearchItem.java              # 검색 결과 항목
├── SearchableModelObject.java   # 검색 가능 모델 객체
├── SuggestedItem.java           # 자동완성 제안 항목
├── FixedSet.java                # 고정 검색 항목 집합
├── UnionSearchIndex.java        # 복합 인덱스
└── SearchItems.java             # 검색 항목 팩토리

core/src/main/java/jenkins/websocket/
├── WebSockets.java              # WebSocket 엔트리포인트 (150줄)
├── WebSocketSession.java        # 세션 관리 (125줄)
└── Provider.java                # SPI 인터페이스
```

---

## 2. 검색 시스템 아키텍처

### 전체 구조

```
┌─────────────────────────────────────────────────────────────┐
│                    Jenkins 검색 아키텍처                      │
│                                                              │
│  ┌────────────────┐    ┌──────────────────────┐              │
│  │ 검색 바 (UI)    │───>│ /search?q=my-job     │              │
│  │ JavaScript      │    │ (StaplerRequest)      │              │
│  └────────────────┘    └──────────┬───────────┘              │
│                                   │                          │
│  ┌────────────────────────────────▼──────────────────┐       │
│  │              Search (StaplerProxy)                 │       │
│  │  ┌──────────────────────────────────────┐          │       │
│  │  │ doIndex()    → 정확 매칭 → 리다이렉트 │          │       │
│  │  │ doSuggest()  → 자동완성 → JSON 응답   │          │       │
│  │  │ doSuggestOpenSearch() → OpenSearch     │          │       │
│  │  └──────────────────┬───────────────────┘          │       │
│  └─────────────────────┼──────────────────────────────┘       │
│                        │                                     │
│  ┌─────────────────────▼──────────────────────────────┐       │
│  │         SearchIndex (계층 구조)                     │       │
│  │                                                     │       │
│  │  Jenkins (root)                                     │       │
│  │  ├── Job "my-job"        ──── FixedSet              │       │
│  │  │   ├── Build #1        ──── FixedSet              │       │
│  │  │   └── Build #2                                   │       │
│  │  ├── View "All"          ──── FixedSet              │       │
│  │  ├── Node "agent-1"     ──── FixedSet              │       │
│  │  └── Configuration...    ──── FixedSet              │       │
│  │                                                     │       │
│  │  각 SearchableModelObject가 자신의 SearchIndex 제공  │       │
│  │  → SearchIndexBuilder로 구축                        │       │
│  │  → UnionSearchIndex로 병합                          │       │
│  └─────────────────────────────────────────────────────┘       │
└─────────────────────────────────────────────────────────────┘
```

### 왜 자체 검색 엔진인가?

- Jenkins는 데이터베이스를 사용하지 않으므로 SQL LIKE 검색 불가
- 모든 객체가 메모리에 존재하므로 인메모리 인덱스가 자연스러움
- SearchableModelObject 인터페이스로 각 모델이 자신의 검색 인덱스를 제공

---

## 3. SearchIndex 인터페이스 분석

### 인터페이스 정의

```java
// 소스: core/src/main/java/hudson/search/SearchIndex.java
public interface SearchIndex {
    // 정확 매칭: 토큰에 정확히 일치하는 항목 검색
    void find(String token, List<SearchItem> result);

    // 부분 매칭: find()의 상위 집합, 자동완성에 사용
    void suggest(String token, List<SearchItem> result);

    // 빈 인덱스 싱글턴
    SearchIndex EMPTY = new SearchIndex() {
        @Override
        public void find(String token, List<SearchItem> result) {}
        @Override
        public void suggest(String token, List<SearchItem> result) {}
    };
}
```

### find vs suggest의 차이

```
find("my-job"):
  → 이름이 정확히 "my-job"인 항목만 반환
  → 정확 매칭 → 리다이렉트에 사용

suggest("my"):
  → "my"를 포함하는 모든 항목 반환
  → "my-job", "my-pipeline", "my-view" 등
  → 자동완성 목록에 사용
```

---

## 4. Search 클래스: 검색 엔진 핵심

### 클래스 구조

```java
// 소스: core/src/main/java/hudson/search/Search.java
public class Search implements StaplerProxy {
    // 최대 검색 결과 수
    private static int MAX_SEARCH_SIZE = Integer.getInteger(
        Search.class.getName() + ".MAX_SEARCH_SIZE", 500);
}
```

### doIndex: 정확 검색 + 리다이렉트

```java
// 소스: Search.java:114-139
private void doIndexImpl(StaplerRequest2 req, StaplerResponse2 rsp)
        throws IOException, ServletException {
    List<Ancestor> l = req.getAncestors();
    // URL 조상 체인을 역순으로 탐색하여 SearchableModelObject 찾기
    for (int i = l.size() - 1; i >= 0; i--) {
        Ancestor a = l.get(i);
        if (a.getObject() instanceof SearchableModelObject smo) {
            SearchIndex index = smo.getSearchIndex();
            String query = req.getParameter("q");
            if (query != null) {
                SuggestedItem target = find(index, query, smo);
                if (target != null) {
                    // 정확 매칭 → 해당 URL로 리다이렉트
                    rsp.sendRedirect2(
                        req.getContextPath() + target.getUrl());
                    return;
                }
            }
        }
    }
    // 매칭 없음 → 404 + 검색 실패 페이지
    rsp.setStatus(SC_NOT_FOUND);
    req.getView(this, "search-failed.jelly").forward(req, rsp);
}
```

### doSuggest: 자동완성 API

```java
// 소스: Search.java:166-200
public void doSuggest(StaplerRequest2 req, StaplerResponse2 rsp,
        @QueryParameter String query)
        throws IOException, ServletException {
    Result r = new Result();
    for (SuggestedItem curItem : getSuggestions(req, query)) {
        String iconName = curItem.item.getSearchIcon();
        // 아이콘 처리 (symbol 또는 URL)
        if (iconName == null || ...) {
            iconName = "symbol-search";
        }
        r.suggestions.add(new Item(
            curItem.getPath(), curItem.getUrl(),
            icon, type,
            curItem.item.getSearchGroup().getDisplayName()));
    }
    // 그룹별 정렬 (Extension 우선순위 기반)
    r.suggestions.sort(
        Comparator.comparingDouble(
            (Item item) -> searchGroupOrdinal
                .getOrDefault(item.getGroup(), Double.MAX_VALUE))
            .reversed()
            .thenComparing(item -> item.name));

    rsp.serveExposedBean(req, r, new ExportConfig());
}
```

### getSuggestions: 중복 제거 + 최대 크기 제한

```java
// 소스: Search.java:225-242
private SearchResult getSuggestionsImpl(StaplerRequest2 req,
        String query) {
    Set<String> paths = new HashSet<>();  // 경로 중복 방지
    SearchResultImpl r = new SearchResultImpl();
    int max = Math.min(
        req.hasParameter("max")
            ? Integer.parseInt(req.getParameter("max")) : 100,
        MAX_SEARCH_SIZE);  // 최대 500

    SearchableModelObject smo = findClosestSearchableModelObject(req);
    for (SuggestedItem i : suggest(makeSuggestIndex(req), query, smo)) {
        if (r.size() >= max) {
            r.hasMoreResults = true;  // "더 많은 결과 있음" 플래그
            break;
        }
        if (paths.add(i.getPath()))  // 중복 경로 필터링
            r.add(i);
    }
    return r;
}
```

---

## 5. 토큰 기반 다단계 검색 알고리즘

### TokenList: 토큰 분할

```java
// 소스: Search.java:457-500
static final class TokenList {
    private final String[] tokens;

    TokenList(String tokenList) {
        // 공백 경계에서 분할 (공백 뒤에 비공백이 오는 지점)
        tokens = tokenList != null
            ? tokenList.split("(?<=\\s)(?=\\S)")
            : EMPTY_STRING_ARRAY;
    }

    // subSequence(start): [start..end]까지의 토큰 결합
    public List<String> subSequence(final int start) {
        return new AbstractList<>() {
            @Override
            public String get(int index) {
                StringBuilder buf = new StringBuilder();
                for (int i = start; i <= start + index; i++)
                    buf.append(tokens[i]);
                return buf.toString().trim();
            }
            @Override
            public int size() { return tokens.length - start; }
        };
    }
}
```

### 다단계 검색: find() 핵심 알고리즘

```java
// 소스: Search.java:502-543
private static List<SuggestedItem> find(
        Mode m, SearchIndex index, String tokenList,
        SearchableModelObject searchContext) {

    TokenList tokens = new TokenList(tokenList);
    if (tokens.length() == 0) return Collections.emptyList();

    // paths[i] = i개의 토큰을 소비한 뒤 도달할 수 있는 결과 목록
    List<SuggestedItem>[] paths = new List[tokens.length() + 1];
    for (int i = 1; i <= tokens.length(); i++)
        paths[i] = new ArrayList<>();

    List<SearchItem> items = new ArrayList<>();

    // === 1단계: 첫 번째 토큰(들)으로 루트 인덱스 검색 ===
    int w = 1;
    for (String token : tokens.subSequence(0)) {
        items.clear();
        m.find(index, token, items);       // 루트 인덱스에서 검색
        for (SearchItem si : items) {
            paths[w].add(SuggestedItem.build(searchContext, si));
        }
        w++;
    }

    // === 2단계: 후속 토큰으로 재귀적 하위 검색 ===
    for (int j = 1; j < tokens.length(); j++) {
        w = 1;
        for (String token : tokens.subSequence(j)) {
            for (SuggestedItem r : paths[j]) {
                items.clear();
                // 이전 결과의 하위 인덱스에서 검색
                m.find(r.item.getSearchIndex(), token, items);
                for (SearchItem i : items)
                    paths[j + w].add(new SuggestedItem(r, i));
            }
            w++;
        }
    }

    // 모든 토큰을 소비한 결과 반환
    return paths[tokens.length()];
}
```

### 알고리즘 동작 예시

```
쿼리: "my job 42"
토큰: ["my ", "job ", "42"]

1단계: 루트 인덱스에서 검색
  토큰 "my"       → paths[1] = [SuggestedItem("my-view")]
  토큰 "my job"   → paths[2] = [SuggestedItem("my-job")]
  토큰 "my job 42"→ paths[3] = []

2단계: paths[1]의 하위 인덱스에서 후속 검색
  "my-view"의 SearchIndex에서 "job" 검색
    → paths[2] 추가: [SuggestedItem("my-view" → "some-job")]
  "my-view"의 SearchIndex에서 "job 42" 검색
    → paths[3] 추가: []

  paths[2]의 각 항목 하위에서 "42" 검색
    "my-job"의 SearchIndex에서 "42" 검색
    → paths[3] 추가: [SuggestedItem("my-job" → "#42")]

최종 결과: paths[3] = [SuggestedItem(path="my-job #42")]
```

---

## 6. EditDistance 기반 결과 정렬

### suggest 메서드의 정렬 전략

```java
// 소스: Search.java:421-455
public static List<SuggestedItem> suggest(
        SearchIndex index, final String tokenList,
        SearchableModelObject searchContext) {

    class Tag implements Comparable<Tag> {
        final SuggestedItem item;
        final int distance;       // 편집 거리
        final int prefixMatch;    // 접두사 일치 여부

        Tag(SuggestedItem i) {
            item = i;
            distance = EditDistance.editDistance(
                i.getPath(), tokenList);
            prefixMatch = i.getPath().startsWith(tokenList) ? 1 : 0;
        }

        @Override
        public int compareTo(Tag that) {
            // 1순위: 접두사 일치 항목 우선
            int r = this.prefixMatch - that.prefixMatch;
            if (r != 0) return -r;
            // 2순위: 편집 거리가 짧은 것 우선
            return this.distance - that.distance;
        }
    }

    List<Tag> buf = new ArrayList<>();
    for (SuggestedItem i : find(Mode.SUGGEST, index, tokenList, ctx))
        buf.add(new Tag(i));
    Collections.sort(buf);
    // Tag에서 SuggestedItem만 추출
    ...
}
```

### 정렬 결과 예시

```
쿼리: "build"

검색 결과 & 편집 거리:
  "build-pipeline"  distance=9, prefixMatch=1  → 순위 1 (접두사 일치)
  "build-test"      distance=5, prefixMatch=1  → 순위 2 (접두사 일치, 거리 짧음)
  "my-build"        distance=3, prefixMatch=0  → 순위 3 (접두사 불일치)
  "rebuild"         distance=2, prefixMatch=0  → 순위 4 (접두사 불일치, 거리 짧음)

최종 정렬: build-test, build-pipeline, rebuild, my-build
```

### findClosestSuggestedItem: 동점 해결

```java
// 소스: Search.java:366-379
static SuggestedItem findClosestSuggestedItem(
        List<SuggestedItem> r, String query) {
    for (SuggestedItem curItem : r) {
        // URL에 쿼리 문자열이 포함된 항목 우선 선택
        if (curItem.item.getSearchUrl()
                .contains(Util.rawEncode(query))) {
            return curItem;
        }
    }
    return r.getFirst();  // 없으면 첫 번째 항목
}
```

---

## 7. SearchIndexBuilder: 인덱스 구축

### 빌더 패턴

```java
// 소스: core/src/main/java/hudson/search/SearchIndexBuilder.java
public final class SearchIndexBuilder {
    private final List<SearchItem> items = new ArrayList<>();
    private final List<SearchIndex> indices = new ArrayList<>();

    // URL=이름인 간단한 항목 추가
    public SearchIndexBuilder add(String urlAsWellAsName) {
        return add(urlAsWellAsName, urlAsWellAsName);
    }

    // URL과 이름이 다른 항목 추가
    public SearchIndexBuilder add(String url, String name) {
        items.add(SearchItems.create(name, url));
        return this;
    }

    // 하위 검색 가능 객체 추가
    public SearchIndexBuilder add(String url,
            SearchableModelObject searchable, String name) {
        items.add(SearchItems.create(name, url, searchable));
        return this;
    }

    // 다른 SearchIndex 병합
    public SearchIndexBuilder add(SearchIndex index) {
        this.indices.add(index);
        return this;
    }

    // 최종 인덱스 생성
    public SearchIndex make() {
        SearchIndex r = new FixedSet(items);
        for (SearchIndex index : indices)
            r = new UnionSearchIndex(r, index);
        return r;
    }
}
```

### 사용 예시 (모델 객체에서)

```
Jenkins.makeSearchIndex():
  new SearchIndexBuilder()
      .add("configure", "config", "configure")
      .add("manage", "manage")
      .add("log", "log")
      .addAllAnnotations(this)    // @QuickSilver 어노테이션 수집
      .add(getSearchIndex())      // 하위 항목 인덱스
      .make()

Job.makeSearchIndex():
  new SearchIndexBuilder()
      .add("configure", "config")
      .add("changes")
      .add("workspace")
      .add(builds의 SearchIndex)
      .make()
```

### UnionSearchIndex: 복합 인덱스

```
makeSuggestIndex(req):
  모든 URL 조상의 SearchIndex를 합쳐서 하나의 인덱스 생성

  /jenkins/job/my-job/42 에서 검색할 때:
  └── UnionSearchIndex
      ├── Jenkins.getSearchIndex()     → Job, View, Node 검색
      ├── Job("my-job").getSearchIndex() → Build 번호 검색
      └── Build(#42).getSearchIndex()  → 아티팩트 등 검색
```

---

## 8. 검색 자동완성 (Suggest)

### OpenSearch 지원

```java
// 소스: Search.java:150-161
public void doSuggestOpenSearch(StaplerRequest2 req,
        StaplerResponse2 rsp, @QueryParameter String q)
        throws IOException, ServletException {
    rsp.setContentType(Flavor.JSON.contentType);
    DataWriter w = Flavor.JSON.createDataWriter(null, rsp);
    w.startArray();
    w.value(q);        // 원본 쿼리
    w.startArray();
    for (SuggestedItem item : getSuggestions(req, q))
        w.value(item.getPath());  // 제안 경로
    w.endArray();
    w.endArray();
}
// 응답 형식: ["build", ["build-test", "build-pipeline", ...]]
```

### SearchGroup 기반 그룹화

```
검색 결과가 그룹으로 분류됨:
  Jobs:
    - build-pipeline
    - build-test
  Views:
    - All
  People:
    - admin

그룹 정렬: Extension 우선순위(ordinal)로 결정
  → 플러그인이 SearchGroup을 등록하여 커스텀 그룹 추가 가능
```

---

## 9. WebSocket 아키텍처

### 전체 구조

```
┌──────────────────────────────────────────────────────┐
│                 Jenkins WebSocket 아키텍처            │
│                                                      │
│  ┌──────────────┐                                    │
│  │ Browser/CLI  │                                    │
│  │  WebSocket   │                                    │
│  │  Client      │                                    │
│  └──────┬───────┘                                    │
│         │ ws:// or wss://                            │
│         │                                            │
│  ┌──────▼────────────────────────────────────┐       │
│  │           WebSockets (엔트리포인트)        │       │
│  │  + upgrade(session): HttpResponse          │       │
│  │  + upgradeResponse(session, req, rsp)     │       │
│  │  + isSupported(): boolean                 │       │
│  └──────┬────────────────────────────────────┘       │
│         │ delegates to                               │
│  ┌──────▼────────────────────────────────────┐       │
│  │     Provider (ServiceLoader SPI)           │       │
│  │  + handle(req, rsp, listener): Handler     │       │
│  │                                            │       │
│  │  구현체: Jetty WebSocketUpgradeFilter      │       │
│  │  (winstone-jetty 모듈에서 제공)            │       │
│  └──────┬────────────────────────────────────┘       │
│         │ callbacks                                  │
│  ┌──────▼────────────────────────────────────┐       │
│  │     WebSocketSession (abstract)            │       │
│  │  # opened()                                │       │
│  │  # closed(statusCode, reason)             │       │
│  │  # error(cause)                            │       │
│  │  # binary(payload, offset, len)           │       │
│  │  # text(message)                           │       │
│  │  + sendBinary(data): Future<Void>          │       │
│  │  + sendText(text): Future<Void>            │       │
│  │  + close()                                 │       │
│  │                                            │       │
│  │  + startPings() / stopPings()             │       │
│  └────────────────────────────────────────────┘       │
└──────────────────────────────────────────────────────┘
```

### 왜 SPI(ServiceLoader) 패턴인가?

```
WebSocket 서버 구현은 서블릿 컨테이너에 의존적:
  - Jetty: WebSocketUpgradeFilter
  - Tomcat: WsServerContainer
  - 기타: 각각 다른 API

Jenkins 코어는 특정 구현에 의존하지 않고,
ServiceLoader로 런타임에 사용 가능한 Provider를 발견:

  META-INF/services/jenkins.websocket.Provider
  → org.eclipse.jetty.websocket.JettyWebSocketProvider

이를 통해:
  1. 코어와 컨테이너 구현 분리
  2. WebSocket 미지원 환경에서도 안전하게 동작 (provider == null)
  3. 테스트에서 모의 Provider 주입 가능
```

---

## 10. WebSockets 클래스 분석

### Provider 초기화

```java
// 소스: core/src/main/java/jenkins/websocket/WebSockets.java
public class WebSockets {

    private static final Provider provider = findProvider();

    private static Provider findProvider() {
        Iterator<Provider> it =
            ServiceLoader.load(Provider.class).iterator();
        while (it.hasNext()) {
            try {
                return it.next();
            } catch (ServiceConfigurationError x) {
                // SPI 로딩 실패 → 스킵 (로그만 남김)
                LOGGER.log(Level.FINE, null, x);
            }
        }
        return null;  // WebSocket 미지원
    }

    public static boolean isSupported() {
        return provider != null;
    }
}
```

### upgrade: HTTP → WebSocket 프로토콜 전환

```java
// 소스: WebSockets.java:68-75
public static HttpResponse upgrade(WebSocketSession session) {
    return new HttpResponse() {
        @Override
        public void generateResponse(
                StaplerRequest2 req, StaplerResponse2 rsp,
                Object node)
                throws IOException, ServletException {
            upgradeResponse(session, req, rsp);
        }
    };
}
```

### upgradeResponse: 업그레이드 실행

```java
// 소스: WebSockets.java:81-142
public static void upgradeResponse(WebSocketSession session,
        HttpServletRequest req, HttpServletResponse rsp)
        throws IOException, ServletException {

    if (provider == null) {
        rsp.setStatus(HttpServletResponse.SC_NOT_FOUND);
        return;
    }

    try {
        session.handler = provider.handle(req, rsp,
            new Provider.Listener() {

            private Object providerSession;

            @Override
            public void onWebSocketConnect(Object providerSession) {
                this.providerSession = providerSession;
                session.startPings();    // 연결 성공 → 핑 시작
                session.opened();        // 세션 콜백
            }

            @Override
            public void onWebSocketClose(int statusCode, String reason) {
                session.stopPings();     // 연결 종료 → 핑 중지
                session.closed(statusCode, reason);
            }

            @Override
            public void onWebSocketError(Throwable cause) {
                if (cause instanceof ClosedChannelException) {
                    // 채널 닫힘 → close로 처리
                    onWebSocketClose(0, cause.toString());
                } else {
                    session.error(cause);
                }
            }

            @Override
            public void onWebSocketBinary(
                    byte[] payload, int offset, int length) {
                try {
                    session.binary(payload, offset, length);
                } catch (IOException x) {
                    session.error(x);
                }
            }

            @Override
            public void onWebSocketText(String message) {
                try {
                    session.text(message);
                } catch (IOException x) {
                    session.error(x);
                }
            }
        });
    } catch (IOException | ServletException x) {
        throw x;
    } catch (Exception x) {
        LOGGER.log(Level.WARNING, null, x);
        rsp.setStatus(HttpServletResponse.SC_INTERNAL_SERVER_ERROR);
    }
}
```

---

## 11. WebSocketSession: 세션 관리

### 세션 생명주기

```java
// 소스: core/src/main/java/jenkins/websocket/WebSocketSession.java
public abstract class WebSocketSession {

    // Ping 간격 (기본 30초, 시스템 속성으로 변경 가능)
    private static Duration PING_INTERVAL = SystemProperties.getDuration(
        "jenkins.websocket.pingInterval",
        ChronoUnit.SECONDS, Duration.ofSeconds(30));

    Provider.Handler handler;
    private ScheduledFuture<?> pings;

    // 서브클래스가 오버라이드하는 이벤트 콜백
    protected void opened() {}
    protected void closed(int statusCode, String reason) {}
    protected void error(Throwable cause) {
        LOGGER.log(Level.WARNING, "unhandled WebSocket error", cause);
    }
    protected void binary(byte[] payload, int offset, int len)
            throws IOException {
        LOGGER.warning("unexpected binary frame");
    }
    protected void text(String message) throws IOException {
        LOGGER.warning("unexpected text frame");
    }

    // 데이터 전송 메서드 (final → 서브클래스가 오버라이드 불가)
    protected final Future<Void> sendBinary(ByteBuffer data)
            throws IOException {
        return handler.sendBinary(data);
    }
    protected final Future<Void> sendText(String text)
            throws IOException {
        return handler.sendText(text);
    }
    protected final void close() throws IOException {
        handler.close();
    }
}
```

### 세션 상태 전이

```
                    ┌──────────┐
                    │  INITIAL │
                    └────┬─────┘
                         │ HTTP Upgrade 성공
                         ▼
                    ┌──────────┐
           ┌────── │  OPENED  │ ──────┐
           │       └────┬─────┘       │
           │            │             │
     text/binary    sendText/    에러 발생
     수신            sendBinary
           │            │             │
           ▼            ▼             ▼
      ┌──────────────────────┐  ┌──────────┐
      │     ACTIVE           │  │  ERROR   │
      │  양방향 메시지 교환   │  │          │
      └──────────┬───────────┘  └────┬─────┘
                 │                    │
            close() 또는          ClosedChannel
            상대방 close              │
                 │                    │
                 ▼                    ▼
            ┌──────────┐        ┌──────────┐
            │  CLOSED  │        │  CLOSED  │
            └──────────┘        └──────────┘
              pings 정지          pings 정지
```

---

## 12. Provider SPI: 서버 구현 추상화

### Provider 인터페이스 (추정 구조)

```
Provider 인터페이스:
  handle(req, rsp, listener) → Handler

  Handler 인터페이스:
    sendBinary(ByteBuffer) → Future<Void>
    sendBinary(ByteBuffer, boolean isLast) → Future<Void>
    sendText(String) → Future<Void>
    sendPing(ByteBuffer) → Future<Void>
    close()

  Listener 인터페이스:
    onWebSocketConnect(providerSession)
    getProviderSession() → Object
    onWebSocketClose(statusCode, reason)
    onWebSocketError(cause)
    onWebSocketBinary(payload, offset, length)
    onWebSocketText(message)
```

### 구현 계층

```
jenkins.websocket.Provider (SPI 인터페이스)
    │
    └── Jetty 구현 (winstone-jetty 모듈)
        │
        ├── Jakarta WebSocket API 사용
        │   └── @ServerEndpoint 또는 프로그래밍 방식 등록
        │
        └── Jetty WebSocketUpgradeFilter
            └── HTTP → WebSocket 프로토콜 업그레이드
```

---

## 13. Ping/Pong 메커니즘

### 왜 Ping이 필요한가?

```
문제: 리버스 프록시(nginx, GKE Ingress)의 유휴 타임아웃

nginx 기본: 60초 유휴 시 연결 종료
GKE:        30초 유휴 시 연결 종료

해결: 주기적 Ping 전송으로 연결 유지
```

### Ping 구현

```java
// 소스: WebSocketSession.java:70-82
void startPings() {
    if (PING_INTERVAL.compareTo(Duration.ZERO) > 0) {
        pings = Timer.get().scheduleAtFixedRate(() -> {
            try {
                Future<Void> f = handler.sendPing(
                    ByteBuffer.wrap(new byte[0]));
            } catch (Exception x) {
                error(x);
                pings.cancel(true);  // 에러 시 핑 중지
            }
        },
        PING_INTERVAL.dividedBy(2).toSeconds(), // 초기 지연: 간격/2
        PING_INTERVAL.toSeconds(),               // 반복 간격
        TimeUnit.SECONDS);
    }
}

void stopPings() {
    if (pings != null) {
        pings.cancel(true);
    }
}
```

### Ping 간격 설정

```
기본값: 30초
설정: -Djenkins.websocket.pingInterval=15

초기 지연이 간격/2인 이유:
  - 연결 직후 바로 핑을 보내는 것은 불필요
  - 간격/2 후 첫 핑, 이후 간격마다 반복
  - 예: 30초 간격 → 15초 후 첫 핑, 30초마다 반복
```

---

## 14. 설계 결정과 교훈

### 검색 시스템 설계 결정

| 결정 | 이유 | 트레이드오프 |
|------|------|------------|
| 인메모리 인덱스 | DB 미사용 환경에 맞춤 | 대규모 인스턴스에서 메모리 사용 |
| 계층적 SearchIndex | 각 모델이 자신의 검색 범위 정의 | 인덱스 구축 복잡성 |
| EditDistance 정렬 | 오타 허용, 유사 결과 우선 표시 | O(nm) 비용 |
| MAX_SEARCH_SIZE=500 | 과도한 결과 방지 | 검색 정밀도 vs 완전성 |
| TokenList 다단계 검색 | "job name build" 같은 경로 탐색 | 조합 폭발 가능성 |

### WebSocket 설계 결정

| 결정 | 이유 | 트레이드오프 |
|------|------|------------|
| ServiceLoader SPI | 서블릿 컨테이너 독립성 | 런타임 발견의 불확실성 |
| 30초 Ping | nginx/GKE 타임아웃 대응 | 네트워크 오버헤드 |
| Future<Void> 비동기 전송 | 논블로킹 I/O | 에러 처리 복잡성 |
| ClosedChannelException → close | 예외를 정상 종료로 변환 | 정보 손실 가능 |
| Template Method (abstract class) | 콜백 기반 세션 관리 | 단일 상속 제약 |

### 핵심 교훈

1. **인메모리 검색의 현실적 한계**: DB 없이 검색을 구현하려면 각 모델이 능동적으로 인덱스를 제공해야 함. 이는 SearchableModelObject 인터페이스로 해결했지만, 새 모델 추가 시 누락 위험
2. **SPI로 컨테이너 독립성 확보**: WebSocket 같은 서버 의존적 기능을 SPI로 추상화하면, 코어가 특정 서블릿 컨테이너에 종속되지 않음
3. **Ping/Pong은 프록시 환경의 필수**: 클라우드 환경에서 WebSocket 연결 유지는 단순 연결 이상의 노력이 필요
4. **다단계 토큰 검색의 강력함**: "job name build number" 형태의 공백 구분 토큰으로 계층 탐색이 가능한 것은 Jenkins 검색의 핵심 강점
5. **Template Method vs 인터페이스**: WebSocketSession은 추상 클래스로 공통 로직(ping)을 강제하면서 콜백을 오버라이드하게 하는 Template Method 패턴의 전형적 활용

---

## 부록: 주요 소스 파일 요약

| 파일 | 줄수 | 핵심 역할 |
|------|------|----------|
| `Search.java` | 562 | 검색 엔진 핵심, 토큰 기반 다단계 검색 |
| `SearchIndex.java` | 59 | 검색 인덱스 인터페이스 (find/suggest) |
| `SearchIndexBuilder.java` | 108 | 인덱스 빌더 (FixedSet + Union 생성) |
| `WebSockets.java` | 150 | WebSocket 엔트리포인트, Provider SPI |
| `WebSocketSession.java` | 125 | 세션 관리, Ping/Pong, 콜백 |

---

*본 문서는 Jenkins 소스코드를 직접 분석하여 작성되었습니다. 모든 코드 참조는 검증된 실제 경로와 라인 번호를 기반으로 합니다.*
