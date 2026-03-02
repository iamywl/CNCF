# C4 Level 2: Container Diagram

```mermaid
graph TB
    subgraph "E-Commerce System"
        WebApp["🌐 Web App<br/>React + Next.js"]
        API["⚙️ API Server<br/>Node.js + Express"]
        Worker["⏰ Worker<br/>Bull Queue"]
        DB[("🗄️ PostgreSQL")]
        Cache[("⚡ Redis")]
    end

    User["👤 사용자"] -->|"HTTPS"| WebApp
    WebApp -->|"REST API"| API
    API -->|"SQL"| DB
    API -->|"Cache R/W"| Cache
    Cache -->|"Job Queue"| Worker
    Worker -->|"SQL"| DB
```

## Container vs Component 구분

| Container | 설명 | 예시 |
|-----------|------|------|
| Web Application | 사용자에게 UI를 제공 | React SPA, Next.js |
| API Application | 비즈니스 로직 처리 | Express, Spring Boot |
| Database | 데이터 영속화 | PostgreSQL, MongoDB |
| Message Broker | 비동기 통신 | RabbitMQ, Redis Queue |
