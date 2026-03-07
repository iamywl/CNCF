# 17. Kafka 보안 시스템 Deep-Dive

## 목차

1. [개요](#1-개요)
2. [보안 프로토콜](#2-보안-프로토콜)
3. [SASL 인증 프레임워크](#3-sasl-인증-프레임워크)
4. [SASL/PLAIN](#4-saslplain)
5. [SASL/SCRAM](#5-saslscram)
6. [SASL/OAUTHBEARER](#6-sasloauthbearer)
7. [SASL/GSSAPI (Kerberos)](#7-saslgssapi-kerberos)
8. [SSL/TLS](#8-ssltls)
9. [KafkaPrincipal](#9-kafkaprincipal)
10. [인가 (Authorization): ACL 모델](#10-인가-authorization-acl-모델)
11. [Authorizer 인터페이스](#11-authorizer-인터페이스)
12. [Delegation Token](#12-delegation-token)
13. [왜(Why) 이렇게 설계했는가](#13-왜why-이렇게-설계했는가)

---

## 1. 개요

Kafka의 보안 시스템은 **인증(Authentication)**, **인가(Authorization)**, **암호화(Encryption)**
세 축으로 구성된다. Java의 SASL(Simple Authentication and Security Layer) 프레임워크와
SSL/TLS를 기반으로 하며, 플러그인 아키텍처를 통해 다양한 인증 메커니즘을 지원한다.

```
소스 파일 위치:
  clients/src/main/java/org/apache/kafka/common/security/        -- 보안 핵심
  clients/src/main/java/org/apache/kafka/common/security/auth/   -- 인증 컨텍스트
  clients/src/main/java/org/apache/kafka/common/security/plain/  -- SASL/PLAIN
  clients/src/main/java/org/apache/kafka/common/security/scram/  -- SASL/SCRAM
  clients/src/main/java/org/apache/kafka/common/security/oauthbearer/ -- OAuth
  clients/src/main/java/org/apache/kafka/common/security/kerberos/   -- Kerberos
  clients/src/main/java/org/apache/kafka/common/security/ssl/    -- SSL/TLS
  clients/src/main/java/org/apache/kafka/common/security/token/  -- Delegation Token
  clients/src/main/java/org/apache/kafka/common/security/authenticator/ -- 인증 처리
  clients/src/main/java/org/apache/kafka/server/authorizer/      -- 인가 인터페이스
  clients/src/main/java/org/apache/kafka/common/acl/             -- ACL 모델
  clients/src/main/java/org/apache/kafka/common/resource/        -- 리소스 타입
```

### 보안 계층 구조

```
+-------------------------------------------------------------------+
|                        클라이언트 요청                               |
+-------------------------------------------------------------------+
         |
         v
+-------------------------------------------------------------------+
|  1. 전송 계층 보안 (Transport Security)                             |
|     - PLAINTEXT: 암호화 없음                                       |
|     - SSL: TLS 암호화                                              |
|     - SASL_PLAINTEXT: SASL 인증 + 평문                             |
|     - SASL_SSL: SASL 인증 + TLS 암호화                             |
+-------------------------------------------------------------------+
         |
         v
+-------------------------------------------------------------------+
|  2. 인증 (Authentication)                                          |
|     - SSL 상호 인증 (mTLS)                                         |
|     - SASL 메커니즘 (PLAIN, SCRAM, OAUTHBEARER, GSSAPI)           |
|     → KafkaPrincipal 생성                                          |
+-------------------------------------------------------------------+
         |
         v
+-------------------------------------------------------------------+
|  3. 인가 (Authorization)                                           |
|     - Authorizer.authorize(principal, operation, resource)         |
|     - ACL 기반 접근 제어                                            |
|     → ALLOW / DENY 결정                                            |
+-------------------------------------------------------------------+
         |
         v
+-------------------------------------------------------------------+
|  4. 요청 처리                                                       |
+-------------------------------------------------------------------+
```

---

## 2. 보안 프로토콜

**소스 파일**: `clients/src/main/java/org/apache/kafka/common/security/auth/SecurityProtocol.java`

```java
public enum SecurityProtocol {
    PLAINTEXT(0, "PLAINTEXT"),       // 인증 없음, 암호화 없음
    SSL(1, "SSL"),                   // SSL/TLS (인증서 기반)
    SASL_PLAINTEXT(2, "SASL_PLAINTEXT"), // SASL 인증, 평문 전송
    SASL_SSL(3, "SASL_SSL");        // SASL 인증 + SSL/TLS 암호화
}
```

### 프로토콜 선택 가이드

```
+-------------------------------------------------------------------+
|  환경              | 추천 프로토콜     | 이유                       |
+-------------------------------------------------------------------+
|  개발/테스트       | PLAINTEXT         | 설정 단순                   |
|  사내 네트워크     | SASL_PLAINTEXT    | 인증만 필요, 암호화 불필요   |
|  공용 네트워크     | SASL_SSL          | 인증 + 암호화 모두 필요      |
|  인증서 기반 환경  | SSL (mTLS)        | PKI 인프라 있을 때           |
+-------------------------------------------------------------------+
```

### 리스너별 프로토콜 설정

하나의 브로커에서 여러 리스너를 서로 다른 보안 프로토콜로 운영할 수 있다:

```
# server.properties
listeners=INTERNAL://0.0.0.0:9092,EXTERNAL://0.0.0.0:9093
listener.security.protocol.map=INTERNAL:SASL_PLAINTEXT,EXTERNAL:SASL_SSL

# 결과:
#   포트 9092: 사내 클라이언트용 (SASL 인증, 평문)
#   포트 9093: 외부 클라이언트용 (SASL 인증 + TLS 암호화)
```

---

## 3. SASL 인증 프레임워크

### SaslServerAuthenticator

**소스 파일**: `clients/src/main/java/org/apache/kafka/common/security/authenticator/SaslServerAuthenticator.java`

```java
package org.apache.kafka.common.security.authenticator;

// SASL 서버 측 인증 처리
// 브로커에서 클라이언트 요청을 인증하는 핵심 클래스
public class SaslServerAuthenticator implements Authenticator {
    // SASL 핸드셰이크 → 메커니즘 선택 → 챌린지-응답 → 인증 완료
}
```

### SASL 인증 흐름

```
클라이언트                              브로커
    |                                    |
    |  1. SaslHandshakeRequest           |
    |  (mechanism: "SCRAM-SHA-256")      |
    | ---------------------------------> |
    |                                    |
    |  2. SaslHandshakeResponse          |
    |  (enabled mechanisms 목록)         |
    | <--------------------------------- |
    |                                    |
    |  3. SaslAuthenticateRequest #1     |
    |  (SASL 초기 응답)                  |
    | ---------------------------------> |
    |                                    |  SASL 서버 처리
    |  4. SaslAuthenticateResponse #1    |
    |  (SASL 챌린지)                     |
    | <--------------------------------- |
    |                                    |
    |  5. SaslAuthenticateRequest #2     |
    |  (SASL 응답)                       |
    | ---------------------------------> |
    |                                    |  인증 성공/실패 판정
    |  6. SaslAuthenticateResponse #2    |
    |  (인증 결과)                        |
    | <--------------------------------- |
    |                                    |
    |  7. 인증 성공 → 일반 Kafka 요청     |
    | =================================> |
```

### AuthenticateCallbackHandler

각 SASL 메커니즘은 자체 CallbackHandler를 제공한다:

```
SASL 메커니즘            CallbackHandler
    |                        |
    PLAIN         →  PlainServerCallbackHandler
    SCRAM-SHA-*   →  ScramServerCallbackHandler
    OAUTHBEARER   →  OAuthBearerValidatorCallbackHandler
    GSSAPI        →  (JDK 기본)
```

**소스 파일**: `clients/src/main/java/org/apache/kafka/common/security/auth/AuthenticateCallbackHandler.java`

---

## 4. SASL/PLAIN

### PlainSaslServer

**소스 파일**: `clients/src/main/java/org/apache/kafka/common/security/plain/PlainSaslServer.java`

SASL/PLAIN은 가장 단순한 인증 메커니즘으로, 사용자명과 비밀번호를 평문으로 전송한다.

```
SASL/PLAIN 인증 메시지 형식:
  [authorizationId] NUL [username] NUL [password]

예: \0admin\0admin-secret
```

### 인증 흐름

```
클라이언트                              브로커
    |                                    |
    |  username + password               |
    |  (단일 메시지로 전송)               |
    | ---------------------------------> |
    |                                    |  PlainSaslServer.evaluateResponse()
    |                                    |    - username/password 추출
    |                                    |    - CallbackHandler로 검증
    |  인증 결과                          |
    | <--------------------------------- |
```

### 왜 PLAIN은 반드시 SSL과 함께 사용해야 하는가

PLAIN은 비밀번호를 **평문**으로 전송한다. SSL/TLS 없이 사용하면:

1. **도청 위험**: 네트워크 스니핑으로 비밀번호 노출
2. **중간자 공격**: 인증 메시지 탈취 가능

```
올바른 사용:  SASL_SSL + PLAIN    (TLS가 암호화)
위험한 사용:  SASL_PLAINTEXT + PLAIN  (비밀번호 평문 노출)
```

### JAAS 설정

```
# server.properties
listener.name.sasl_ssl.plain.sasl.jaas.config=\
  org.apache.kafka.common.security.plain.PlainLoginModule required \
  username="admin" \
  password="admin-secret" \
  user_admin="admin-secret" \
  user_alice="alice-secret";
```

---

## 5. SASL/SCRAM

### ScramLoginModule

**소스 파일**: `clients/src/main/java/org/apache/kafka/common/security/scram/ScramLoginModule.java`

```java
public class ScramLoginModule implements LoginModule {
    private static final String USERNAME_CONFIG = "username";
    private static final String PASSWORD_CONFIG = "password";
    public static final String TOKEN_AUTH_CONFIG = "tokenauth";

    static {
        ScramSaslClientProvider.initialize();
        ScramSaslServerProvider.initialize();
    }

    @Override
    public void initialize(Subject subject, CallbackHandler callbackHandler,
                          Map<String, ?> sharedState, Map<String, ?> options) {
        String username = (String) options.get(USERNAME_CONFIG);
        if (username != null)
            subject.getPublicCredentials().add(username);
        String password = (String) options.get(PASSWORD_CONFIG);
        if (password != null)
            subject.getPrivateCredentials().add(password);
    }
}
```

### SCRAM 챌린지-응답 프로토콜

SCRAM(Salted Challenge Response Authentication Mechanism)은 비밀번호를 네트워크로
전송하지 않는 안전한 인증 프로토콜이다.

```
지원 메커니즘:
  - SCRAM-SHA-256 (SHA-256 해시)
  - SCRAM-SHA-512 (SHA-512 해시)
```

**소스 파일**: `clients/src/main/java/org/apache/kafka/common/security/scram/internals/ScramMechanism.java`

### SCRAM 인증 흐름

```
클라이언트                              브로커
    |                                    |
    |  client-first-message              |
    |  (username, nonce_c)               |
    | ---------------------------------> |
    |                                    |  salt, iterations 조회 (메타데이터)
    |  server-first-message              |
    |  (salt, iterations, nonce_s)       |
    | <--------------------------------- |
    |                                    |
    |  클라이언트:                        |
    |  SaltedPassword = PBKDF2(          |
    |    password, salt, iterations)      |
    |  ClientKey = HMAC(SaltedPassword,  |
    |    "Client Key")                   |
    |  StoredKey = SHA(ClientKey)        |
    |  ClientSignature = HMAC(StoredKey, |
    |    AuthMessage)                    |
    |  ClientProof = ClientKey XOR       |
    |    ClientSignature                 |
    |                                    |
    |  client-final-message              |
    |  (ClientProof, nonce)              |
    | ---------------------------------> |
    |                                    |  브로커:
    |                                    |  StoredKey 검증
    |                                    |  ServerSignature 계산
    |  server-final-message              |
    |  (ServerSignature)                 |
    | <--------------------------------- |
    |                                    |
    |  클라이언트: ServerSignature 검증   |
    |  (상호 인증 완료)                   |
```

### SCRAM이 PLAIN보다 안전한 이유

| 항목 | PLAIN | SCRAM |
|------|-------|-------|
| 비밀번호 전송 | 평문 전송 | 전송 안함 (proof만 전송) |
| 재전송 공격 | 취약 | nonce로 방어 |
| 서버 저장 | 평문 또는 해시 | salt + iteration + StoredKey |
| 상호 인증 | 서버만 검증 | 클라이언트도 서버 검증 |
| SSL 필수 | 필수 | 권장 (없어도 안전) |

### SCRAM 자격증명 관리

```
# 사용자 생성
kafka-configs.sh --bootstrap-server localhost:9092 \
  --alter --add-config 'SCRAM-SHA-256=[iterations=8192,password=alice-secret]' \
  --entity-type users --entity-name alice

# 내부 저장 형식 (KRaft 메타데이터):
UserScramCredentialRecord {
  name: "alice",
  mechanism: SCRAM-SHA-256,
  salt: <random bytes>,
  storedKey: <derived key>,
  serverKey: <derived key>,
  iterations: 8192
}
```

---

## 6. SASL/OAUTHBEARER

### OAuthBearerLoginModule

**소스 파일**: `clients/src/main/java/org/apache/kafka/common/security/oauthbearer/OAuthBearerLoginModule.java`

SASL/OAUTHBEARER는 OAuth 2.0 / JWT 토큰 기반 인증을 제공한다.

```java
// OAuthBearerLoginModule은 JAAS LoginModule 구현
// AuthenticateCallbackHandler를 통해 토큰 획득
// OAuthBearerToken 인터페이스로 토큰 추상화
```

### OAuthBearerToken 인터페이스

**소스 파일**: `clients/src/main/java/org/apache/kafka/common/security/oauthbearer/OAuthBearerToken.java`

```java
public interface OAuthBearerToken {
    String value();                     // JWT 토큰 문자열
    Set<String> scope();                // OAuth 스코프
    long lifetimeMs();                  // 만료 시각 (밀리초)
    String principalName();             // 주체 이름
    Long startTimeMs();                 // 발급 시각
}
```

### OAuth 인증 흐름

```
+-------------------+                 +-------------------+
|   Kafka 클라이언트 |                 |    OAuth Server   |
|                   |  1. 토큰 요청    |   (Keycloak 등)   |
|                   | --------------> |                   |
|                   |                 |                   |
|                   |  2. JWT 토큰    |                   |
|                   | <-------------- |                   |
+--------+----------+                 +-------------------+
         |
         |  3. JWT 토큰으로 Kafka 인증
         |
         v
+-------------------+
|   Kafka 브로커    |
|                   |
|  JwtValidator:    |
|  - 서명 검증      |
|  - 만료 확인      |
|  - 클레임 추출    |
|  - 주체 식별      |
+-------------------+
```

### JwtValidator

**소스 파일**: `clients/src/main/java/org/apache/kafka/common/security/oauthbearer/JwtValidator.java`

```
JWT 토큰 검증 과정:
    |
    +---> 1. 토큰 파싱 (header.payload.signature)
    |
    +---> 2. 서명 검증
    |         - JWKS(JSON Web Key Set) 엔드포인트에서 공개키 조회
    |         - 또는 로컬 인증서로 검증
    |
    +---> 3. 클레임 검증
    |         - exp (만료 시간) 확인
    |         - iss (발급자) 확인
    |         - aud (대상) 확인
    |         - scope 확인
    |
    +---> 4. KafkaPrincipal 생성
              - principalName = sub 클레임 (또는 설정된 클레임)
```

### 왜 OAUTHBEARER를 추가했는가

1. **기업 SSO 통합**: 기존 OAuth/OIDC 인프라를 재사용
2. **토큰 기반 인증**: 비밀번호 없이 단기 토큰으로 인증
3. **세밀한 권한**: OAuth 스코프로 세밀한 접근 제어 가능
4. **토큰 갱신**: 클라이언트가 자동으로 토큰 갱신 가능

---

## 7. SASL/GSSAPI (Kerberos)

### Kerberos 인증

SASL/GSSAPI는 Kerberos 프로토콜을 사용하는 엔터프라이즈 인증 메커니즘이다.

**소스 파일**: `clients/src/main/java/org/apache/kafka/common/security/kerberos/KerberosShortNamer.java`

```
Kerberos 인증 흐름:
+----------+         +----------+         +----------+
| 클라이언트|         |   KDC    |         | 브로커   |
|          |         | (Key     |         |          |
|          |         | Distrib. |         |          |
|          |         | Center)  |         |          |
+----+-----+         +----+-----+         +----+-----+
     |                     |                    |
     |  1. AS-REQ          |                    |
     |  (TGT 요청)         |                    |
     | ------------------> |                    |
     |                     |                    |
     |  2. AS-REP (TGT)   |                    |
     | <------------------ |                    |
     |                     |                    |
     |  3. TGS-REQ         |                    |
     |  (서비스 티켓 요청)  |                    |
     | ------------------> |                    |
     |                     |                    |
     |  4. TGS-REP         |                    |
     |  (서비스 티켓)       |                    |
     | <------------------ |                    |
     |                     |                    |
     |  5. AP-REQ (서비스 티켓으로 인증)          |
     | ----------------------------------------> |
     |                     |                    |
     |  6. AP-REP (상호 인증)                    |
     | <---------------------------------------- |
```

### KerberosShortNamer

Kerberos principal(예: `kafka/broker1.example.com@EXAMPLE.COM`)을
짧은 이름(예: `kafka`)으로 변환하는 규칙을 관리한다.

```
# sasl.kerberos.principal.to.local.rules 설정 예시
RULE:[2:$1@$0](kafka/.*@EXAMPLE.COM)s/.*/kafka/
DEFAULT

# 변환 결과:
# kafka/broker1.example.com@EXAMPLE.COM → kafka
# alice@EXAMPLE.COM → alice
```

### Kerberos 설정

```
# server.properties
sasl.enabled.mechanisms=GSSAPI
sasl.kerberos.service.name=kafka
listeners=SASL_PLAINTEXT://0.0.0.0:9092

# JAAS 설정 (kafka_server_jaas.conf)
KafkaServer {
    com.sun.security.auth.module.Krb5LoginModule required
    useKeyTab=true
    storeKey=true
    keyTab="/etc/security/keytabs/kafka.service.keytab"
    principal="kafka/broker1.example.com@EXAMPLE.COM";
};
```

---

## 8. SSL/TLS

### SslFactory

**소스 파일**: `clients/src/main/java/org/apache/kafka/common/security/ssl/SslFactory.java`

```java
public class SslFactory implements Reconfigurable, Closeable {
    private final ConnectionMode connectionMode;  // CLIENT 또는 SERVER
    private final String clientAuthConfigOverride;
    private final boolean keystoreVerifiableUsingTruststore;
    private String endpointIdentification;
    private SslEngineFactory sslEngineFactory;
}
```

### SSL/TLS 구성 요소

```
+-------------------------------------------------------------------+
|  SSL/TLS 설정                                                      |
|                                                                   |
|  Keystore (ssl.keystore.*)                                        |
|    - 브로커/클라이언트의 개인키 + 인증서                              |
|    - 형식: JKS, PKCS12, PEM                                        |
|                                                                   |
|  Truststore (ssl.truststore.*)                                    |
|    - 신뢰할 CA 인증서 목록                                          |
|    - 상대방 인증서 검증에 사용                                       |
|                                                                   |
|  클라이언트 인증 (ssl.client.auth)                                  |
|    - none: 클라이언트 인증서 불요                                    |
|    - requested: 요청하지만 필수 아님                                 |
|    - required: 필수 (mTLS)                                         |
+-------------------------------------------------------------------+
```

### 상호 인증 (mTLS) 흐름

```
클라이언트                              브로커
    |                                    |
    |  ClientHello                        |
    | ---------------------------------> |
    |                                    |
    |  ServerHello + 서버 인증서           |
    |  + CertificateRequest (mTLS 시)    |
    | <--------------------------------- |
    |                                    |
    |  클라이언트 인증서 검증:             |
    |  1. 서버 인증서가 Truststore에 있는가?
    |  2. 호스트명이 일치하는가?           |
    |     (ssl.endpoint.identification    |
    |      .algorithm = HTTPS)           |
    |                                    |
    |  클라이언트 인증서 + KeyExchange     |
    | ---------------------------------> |
    |                                    |
    |  브로커 인증서 검증:                 |
    |  1. 클라이언트 인증서가 Truststore에 있는가?
    |                                    |
    |  Finished (암호화 채널 수립)         |
    | <================================> |
```

### 동적 SSL 인증서 갱신

SslFactory는 `Reconfigurable` 인터페이스를 구현하여 브로커 재시작 없이
인증서를 갱신할 수 있다:

```
# 동적 갱신 흐름
1. kafka-configs.sh로 새 keystore/truststore 경로 설정
2. SslFactory.reconfigure() 호출
3. 새 SSLEngine 생성 시 새 인증서 사용
4. 기존 연결은 유지, 새 연결부터 새 인증서 적용
```

---

## 9. KafkaPrincipal

**소스 파일**: `clients/src/main/java/org/apache/kafka/common/security/auth/KafkaPrincipal.java`

```java
public class KafkaPrincipal implements Principal {
    public static final String USER_TYPE = "User";
    public static final KafkaPrincipal ANONYMOUS =
        new KafkaPrincipal(KafkaPrincipal.USER_TYPE, "ANONYMOUS");

    private final String principalType;     // "User" (기본)
    private final String name;              // 사용자 이름
    private volatile boolean tokenAuthenticated;  // Delegation Token 인증 여부

    public KafkaPrincipal(String principalType, String name) {
        this(principalType, name, false);
    }

    public KafkaPrincipal(String principalType, String name,
                          boolean tokenAuthenticated) {
        this.principalType = requireNonNull(principalType);
        this.name = requireNonNull(name);
        this.tokenAuthenticated = tokenAuthenticated;
    }
}
```

### Principal 생성 과정

```
인증 메커니즘에 따른 KafkaPrincipal 생성:

SASL/PLAIN:
  username "alice" → KafkaPrincipal("User", "alice")

SASL/SCRAM:
  username "bob" → KafkaPrincipal("User", "bob")

SASL/OAUTHBEARER:
  JWT sub claim "service-account" → KafkaPrincipal("User", "service-account")

SASL/GSSAPI:
  principal "alice@EXAMPLE.COM" → KerberosShortNamer →
  KafkaPrincipal("User", "alice")

SSL (mTLS):
  CN=admin,OU=Kafka,O=Company → KafkaPrincipal("User", "CN=admin,OU=Kafka,O=Company")

PLAINTEXT (인증 없음):
  → KafkaPrincipal.ANONYMOUS ("User", "ANONYMOUS")
```

### KafkaPrincipalBuilder

**소스 파일**: `clients/src/main/java/org/apache/kafka/common/security/auth/KafkaPrincipalBuilder.java`

사용자 정의 Principal 생성을 위한 인터페이스:

```java
public interface KafkaPrincipalBuilder {
    KafkaPrincipal build(AuthenticationContext context);
}
```

### 인증 컨텍스트 타입

| 프로토콜 | 컨텍스트 | 설명 |
|----------|----------|------|
| PLAINTEXT | PlaintextAuthenticationContext | IP 주소만 |
| SSL | SslAuthenticationContext | 인증서 정보 |
| SASL_* | SaslAuthenticationContext | SASL 서버, 메커니즘 |

---

## 10. 인가 (Authorization): ACL 모델

### ResourceType

**소스 파일**: `clients/src/main/java/org/apache/kafka/common/resource/ResourceType.java`

```java
public enum ResourceType {
    UNKNOWN((byte) 0),
    ANY((byte) 1),            // 필터링용 와일드카드
    TOPIC((byte) 2),          // Kafka 토픽
    GROUP((byte) 3),          // 컨슈머 그룹
    CLUSTER((byte) 4),        // 클러스터 전체
    TRANSACTIONAL_ID((byte) 5), // 트랜잭션 ID
    DELEGATION_TOKEN((byte) 6), // 위임 토큰
    USER((byte) 7);           // 사용자 주체
}
```

### AclOperation

**소스 파일**: `clients/src/main/java/org/apache/kafka/common/acl/AclOperation.java`

```java
public enum AclOperation {
    UNKNOWN((byte) 0),
    ANY((byte) 1),              // 필터링용 와일드카드
    ALL((byte) 2),              // 모든 작업
    READ((byte) 3),             // 읽기 (토픽 소비, 그룹 조회)
    WRITE((byte) 4),            // 쓰기 (토픽 프로듀스)
    CREATE((byte) 5),           // 생성 (토픽 생성)
    DELETE((byte) 6),           // 삭제 (토픽 삭제, 레코드 삭제)
    ALTER((byte) 7),            // 변경 (파티션 추가, 설정 변경)
    DESCRIBE((byte) 8),         // 조회 (토픽 메타데이터)
    CLUSTER_ACTION((byte) 9),   // 클러스터 작업 (복제, 리더 선출)
    DESCRIBE_CONFIGS((byte) 10), // 설정 조회
    ALTER_CONFIGS((byte) 11),    // 설정 변경
    IDEMPOTENT_WRITE((byte) 12), // 멱등 쓰기
    CREATE_TOKENS((byte) 13),    // 토큰 생성
    DESCRIBE_TOKENS((byte) 14),  // 토큰 조회
    TWO_PHASE_COMMIT((byte) 15); // 2PC 커밋
}
```

### 작업 간 함축 관계

```
AclOperation 함축 관계 (ALLOW 기준):

  ALL ──────────────────────────────────→ 모든 작업
                                          |
  READ ─────────→ DESCRIBE                |
  WRITE ────────→ DESCRIBE                |
  DELETE ───────→ DESCRIBE                |
  ALTER ────────→ DESCRIBE                |
  ALTER_CONFIGS → DESCRIBE_CONFIGS        |
```

### AclBinding

**소스 파일**: `clients/src/main/java/org/apache/kafka/common/acl/AclBinding.java`

```java
public class AclBinding {
    private final ResourcePattern pattern;     // 리소스 패턴
    private final AccessControlEntry entry;    // 접근 제어 항목

    // ResourcePattern: {resourceType, name, patternType(LITERAL|PREFIXED)}
    // AccessControlEntry: {principal, host, operation, permissionType(ALLOW|DENY)}
}
```

### ACL 예시

```
+-------------------------------------------------------------------+
| ACL 규칙                                                           |
+-------------------------------------------------------------------+
| "User:alice"가 토픽 "orders"에 READ 허용                           |
|                                                                   |
| AclBinding(                                                       |
|   pattern: ResourcePattern(TOPIC, "orders", LITERAL),             |
|   entry: AccessControlEntry("User:alice", "*", READ, ALLOW)      |
| )                                                                 |
+-------------------------------------------------------------------+
| "User:bob"가 토픽 "test-*" 프리픽스에 모든 작업 허용                |
|                                                                   |
| AclBinding(                                                       |
|   pattern: ResourcePattern(TOPIC, "test-", PREFIXED),             |
|   entry: AccessControlEntry("User:bob", "*", ALL, ALLOW)         |
| )                                                                 |
+-------------------------------------------------------------------+
| 모든 사용자에게 그룹 "my-group" DESCRIBE 거부                       |
|                                                                   |
| AclBinding(                                                       |
|   pattern: ResourcePattern(GROUP, "my-group", LITERAL),           |
|   entry: AccessControlEntry("User:*", "*", DESCRIBE, DENY)       |
| )                                                                 |
+-------------------------------------------------------------------+
```

### ACL 평가 순서

```
요청: User:alice가 Topic:orders에 READ
    |
    +---> 1. DENY 규칙 검사 (우선순위 높음)
    |         - DENY ALL on Topic:orders for User:alice?
    |         - DENY READ on Topic:orders for User:alice?
    |         - DENY ALL on Topic:orders for User:*?
    |
    +---> 2. ALLOW 규칙 검사
    |         - ALLOW ALL on Topic:orders for User:alice?
    |         - ALLOW READ on Topic:orders for User:alice?
    |         - 함축: ALLOW ALL → READ도 허용
    |
    +---> 3. 기본 정책
              - allow.everyone.if.no.acl.found (기본: true → 허용)
              - super.users에 포함되면 항상 허용
```

### 리소스별 필요한 작업

| 작업 | 리소스 | 필요 ACL |
|------|--------|----------|
| 토픽 소비 | TOPIC | READ |
| 컨슈머 그룹 참여 | GROUP | READ |
| 토픽 프로듀스 | TOPIC | WRITE |
| 트랜잭션 프로듀스 | TRANSACTIONAL_ID | WRITE |
| 토픽 생성 | CLUSTER | CREATE |
| 토픽 설정 조회 | TOPIC | DESCRIBE_CONFIGS |
| 토픽 설정 변경 | TOPIC | ALTER_CONFIGS |
| 토픽 삭제 | TOPIC | DELETE |

---

## 11. Authorizer 인터페이스

**소스 파일**: `clients/src/main/java/org/apache/kafka/server/authorizer/Authorizer.java`

```java
public interface Authorizer extends Configurable, Closeable {
    // 브로커 시작 시 호출 -- 메타데이터 로딩 완료까지 Future 유지
    Map<Endpoint, ? extends CompletionStage<Void>> start(
        AuthorizerServerInfo serverInfo);

    // 인가 검사 -- 핵심 메서드
    List<AuthorizationResult> authorize(
        AuthorizableRequestContext requestContext,
        List<Action> actions);

    // ACL 생성
    List<? extends CompletionStage<AclCreateResult>> createAcls(
        AuthorizableRequestContext requestContext,
        List<AclBinding> aclBindings);

    // ACL 삭제
    List<? extends CompletionStage<AclDeleteResult>> deleteAcls(
        AuthorizableRequestContext requestContext,
        List<AclBindingFilter> aclBindingFilters);

    // ACL 조회
    Iterable<AclBinding> acls(AclBindingFilter filter);
}
```

### Authorizer 구현체

```
Authorizer (인터페이스)
    |
    +---> StandardAuthorizer
    |     - KRaft 모드의 기본 구현
    |     - AclControlManager의 메타데이터 사용
    |     - 메타데이터 로그에서 ACL 로드
    |
    +---> AclAuthorizer (레거시)
    |     - ZooKeeper 기반 ACL 저장
    |
    +---> 사용자 정의 구현
          - authorizer.class.name 설정으로 지정
```

### authorize() 메서드 동작

```
authorize(requestContext, actions)
    |
    +---> requestContext:
    |         - principal: KafkaPrincipal("User", "alice")
    |         - clientAddress: InetAddress
    |         - listenerName: "SASL_SSL"
    |
    +---> actions (체크할 작업 목록):
    |         - Action(READ, ResourcePattern(TOPIC, "orders", LITERAL), ...)
    |
    +---> 각 Action에 대해:
              |
              +---> 1. super.users 체크 → 즉시 ALLOW
              +---> 2. DENY ACL 매칭 → DENIED
              +---> 3. ALLOW ACL 매칭 → ALLOWED
              +---> 4. 기본 정책 적용
              |
              +---> AuthorizationResult.ALLOWED 또는 DENIED
```

---

## 12. Delegation Token

### DelegationTokenManager

Delegation Token은 Kerberos나 SCRAM 자격증명을 공유하지 않고
경량 토큰으로 인증할 수 있게 해주는 메커니즘이다.

```
Delegation Token 사용 시나리오:
+-------------------------------------------------------------------+
|                                                                   |
|  1. 마스터 자격증명 보유자 (예: MapReduce Job Launcher)             |
|     - SCRAM/Kerberos로 인증                                       |
|     - Delegation Token 생성 요청                                   |
|                                                                   |
|  2. 토큰을 워커 노드에 분배                                        |
|     - 실제 비밀번호/keytab 배포 불필요                              |
|                                                                   |
|  3. 워커 노드들이 토큰으로 인증                                     |
|     - SCRAM 메커니즘 + tokenauth=true                              |
|     - 토큰 HMAC로 인증                                             |
|                                                                   |
+-------------------------------------------------------------------+
```

### 토큰 생성 흐름

```
클라이언트 (마스터 자격증명으로 인증됨)
    |
    +---> CreateDelegationToken 요청
    |         - owner: 토큰 소유자
    |         - maxLifeTimeMs: 최대 수명
    |         - renewers: 갱신 허용 주체 목록
    |
    v
컨트롤러
    |
    +---> DelegationTokenControlManager.createToken()
    |         - tokenId 생성 (UUID)
    |         - HMAC 키 생성 (랜덤)
    |         - DelegationTokenRecord 메타데이터에 기록
    |
    +---> 응답: CreateDelegationTokenResponse
              - tokenId
              - hmac (비밀 키 -- 클라이언트에만 전달)
              - expiryTimestamp
```

### 토큰 기반 인증

```
# 클라이언트 JAAS 설정 (토큰 사용)
KafkaClient {
    org.apache.kafka.common.security.scram.ScramLoginModule required
    username="tokenID"
    password="base64-encoded-hmac"
    tokenauth="true";
};

인증 시:
  SCRAM 프로토콜 동일하게 수행
  단, tokenauth=true이면 password를 HMAC 키로 해석
  브로커가 메타데이터에서 토큰 유효성 검증
```

### 토큰 갱신과 만료

```
토큰 생명주기:
  |
  +---> 생성 (CreateDelegationToken)
  |         - issueTimestamp
  |         - expiryTimestamp = issueTimestamp + maxLifeTimeMs
  |
  +---> 갱신 (RenewDelegationToken) -- renewers만 가능
  |         - expiryTimestamp 연장 (maxLifeTimeMs 이내)
  |
  +---> 만료 (ExpireDelegationToken)
  |         - 명시적 만료 또는 자동 만료
  |         - expiryTimestamp 경과 시 자동 무효화
  |
  +---> 삭제
          - 만료된 토큰은 주기적으로 정리
```

---

## 13. 왜(Why) 이렇게 설계했는가

### Q: 왜 Java SASL 프레임워크를 사용하는가?

1. **표준 준수**: SASL은 RFC 4422 표준, 다양한 메커니즘 플러그인 가능
2. **JDK 내장**: 추가 라이브러리 없이 JDK만으로 Kerberos, PLAIN 등 지원
3. **확장성**: 커스텀 메커니즘을 SaslServer/SaslClient로 구현 가능
4. **기업 통합**: 기존 LDAP, Active Directory, Kerberos 인프라와 통합

### Q: 왜 여러 SASL 메커니즘을 동시에 지원하는가?

하나의 브로커가 여러 인증 방식을 동시에 제공해야 하는 실제 사례:

```
리스너 INTERNAL (포트 9092):
  - SASL/SCRAM -- 내부 서비스 인증
  - SASL/PLAIN -- 레거시 시스템 호환

리스너 EXTERNAL (포트 9093):
  - SASL/OAUTHBEARER -- 외부 클라이언트 (OAuth 토큰)
  - SSL (mTLS) -- 파트너 시스템 (인증서)
```

### Q: 왜 ACL에서 DENY가 ALLOW보다 우선순위가 높은가?

보안에서 "deny by default" 원칙:

1. **안전한 기본값**: 명시적 허용 없이는 접근 불가
2. **DENY로 예외 처리**: 넓은 ALLOW 후 특정 사용자/리소스만 차단
3. **실수 방지**: 잘못된 ALLOW 규칙이 있어도 DENY로 보호

```
예: 모든 사용자에게 orders 토픽 읽기 허용, 단 guest 제외

AclBinding(TOPIC, "orders", ALLOW READ for User:*)     -- 전체 허용
AclBinding(TOPIC, "orders", DENY READ for User:guest)  -- guest 차단

→ DENY가 우선이므로 guest는 차단됨
```

### Q: 왜 Delegation Token이 필요한가?

대규모 분산 처리(Spark, Flink)에서:

1. **자격증명 배포 문제**: 수천 개 워커에 Kerberos keytab 배포는 보안 위험
2. **수명 제어**: 토큰에 만료 시간을 설정하여 위험 최소화
3. **갱신 가능**: 장시간 작업에서 토큰 갱신으로 재인증 없이 계속 사용
4. **취소 가능**: 토큰을 즉시 무효화하여 접근 차단

```
Kerberos keytab 배포 vs Delegation Token:

[keytab 방식]                    [토큰 방식]
Job Launcher                     Job Launcher
  |                                |
  +---> keytab 파일 복사            +---> 토큰 생성
  |     (보안 위험!)                |     (HMAC만 전달)
  v                                v
Worker-1: keytab 보유              Worker-1: 토큰 보유
Worker-2: keytab 보유              Worker-2: 토큰 보유
Worker-3: keytab 보유              Worker-3: 토큰 보유
  ↓                                  ↓
keytab 유출 = 영구 접근             토큰 만료 = 자동 차단
```

### Q: 왜 SSL 인증서 동적 갱신을 지원하는가?

인증서는 유효 기간이 있어 주기적 갱신이 필요하다:

1. **무중단 운영**: 브로커 재시작 없이 인증서 교체
2. **자동화**: Let's Encrypt 같은 자동 인증서 갱신과 통합
3. **보안**: 짧은 유효 기간의 인증서로 보안 강화 가능

---

## 부록: 주요 소스 파일 색인

| 파일 | 경로 | 설명 |
|------|------|------|
| SecurityProtocol.java | clients/.../security/auth/SecurityProtocol.java | 보안 프로토콜 열거형 |
| KafkaPrincipal.java | clients/.../security/auth/KafkaPrincipal.java | 인증 주체 |
| KafkaPrincipalBuilder.java | clients/.../security/auth/KafkaPrincipalBuilder.java | 주체 빌더 인터페이스 |
| SaslServerAuthenticator.java | clients/.../security/authenticator/SaslServerAuthenticator.java | SASL 서버 인증 |
| PlainSaslServer.java | clients/.../security/plain/PlainSaslServer.java | PLAIN 메커니즘 |
| ScramLoginModule.java | clients/.../security/scram/ScramLoginModule.java | SCRAM 로그인 모듈 |
| ScramMechanism.java | clients/.../security/scram/internals/ScramMechanism.java | SCRAM 메커니즘 |
| OAuthBearerLoginModule.java | clients/.../security/oauthbearer/OAuthBearerLoginModule.java | OAuth 로그인 |
| OAuthBearerToken.java | clients/.../security/oauthbearer/OAuthBearerToken.java | OAuth 토큰 인터페이스 |
| JwtValidator.java | clients/.../security/oauthbearer/JwtValidator.java | JWT 검증기 |
| KerberosShortNamer.java | clients/.../security/kerberos/KerberosShortNamer.java | Kerberos 이름 변환 |
| SslFactory.java | clients/.../security/ssl/SslFactory.java | SSL 팩토리 |
| Authorizer.java | clients/.../server/authorizer/Authorizer.java | 인가 인터페이스 |
| AclOperation.java | clients/.../common/acl/AclOperation.java | ACL 작업 |
| AclBinding.java | clients/.../common/acl/AclBinding.java | ACL 바인딩 |
| ResourceType.java | clients/.../common/resource/ResourceType.java | 리소스 타입 |
| AclControlManager.java | metadata/.../controller/AclControlManager.java | ACL 관리 (컨트롤러) |
| DelegationTokenControlManager.java | metadata/.../controller/DelegationTokenControlManager.java | 토큰 관리 |
