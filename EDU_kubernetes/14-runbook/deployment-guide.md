# 배포 가이드 (Deployment Guide)

> 마지막 업데이트: 2024-03-01
> 작성자: DevOps 팀

## 개요

이 문서는 E-Commerce API 서버의 배포 절차를 설명합니다.
스테이징과 프로덕션 환경에 모두 적용됩니다.

## 사전 요구사항

- [ ] Docker >= 24.0
- [ ] kubectl 설정 완료 (클러스터 접근 권한)
- [ ] GitHub 리포지토리 push 권한
- [ ] Slack `#deploy-alerts` 채널 접근

## 배포 파이프라인

```
코드 푸시 → CI (테스트) → Docker 빌드 → Registry Push → K8s 배포 → 헬스 체크
```

---

## 1단계: 코드 준비

```bash
# 최신 main 브랜치 확인
git checkout main
git pull origin main

# 배포할 커밋 확인
git log --oneline -5
```

**확인 사항:**
- [ ] 모든 PR이 머지되었는가
- [ ] CI 파이프라인이 통과했는가 (GitHub Actions 확인)

---

## 2단계: Docker 이미지 빌드

```bash
# 버전 태깅 (semantic versioning)
export VERSION=v1.2.3

# 이미지 빌드
docker build -t ecommerce-api:${VERSION} .

# 로컬 테스트
docker run --rm -p 3000:3000 ecommerce-api:${VERSION}
curl http://localhost:3000/health
# 기대 응답: {"status":"ok"}
```

---

## 3단계: 이미지 푸시

```bash
# Registry에 푸시
docker tag ecommerce-api:${VERSION} registry.example.com/ecommerce-api:${VERSION}
docker push registry.example.com/ecommerce-api:${VERSION}
```

**확인:**
```bash
# 이미지가 Registry에 존재하는지 확인
docker manifest inspect registry.example.com/ecommerce-api:${VERSION}
```

---

## 4단계: 스테이징 배포

```bash
# 스테이징 컨텍스트 전환
kubectl config use-context staging

# 이미지 태그 업데이트
kubectl set image deployment/ecommerce-api \
  api=registry.example.com/ecommerce-api:${VERSION} \
  -n ecommerce

# 배포 상태 확인 (최대 5분 대기)
kubectl rollout status deployment/ecommerce-api -n ecommerce --timeout=300s
```

**스테이징 검증:**
```bash
# 헬스 체크
curl https://api-staging.example.com/health

# 스모크 테스트 (핵심 API 호출)
curl https://api-staging.example.com/api/products
curl -X POST https://api-staging.example.com/api/auth/login \
  -H "Content-Type: application/json" \
  -d '{"email":"test@test.com","password":"test1234"}'
```

- [ ] 헬스 체크 통과
- [ ] 상품 목록 조회 정상
- [ ] 로그인 정상
- [ ] Sentry 에러 확인 (새로운 에러 없는지)

---

## 5단계: 프로덕션 배포

> **주의**: 프로덕션 배포는 반드시 2명 이상이 확인한 후 진행

```bash
# 프로덕션 컨텍스트 전환
kubectl config use-context production

# 배포 전 현재 상태 기록 (롤백 대비)
kubectl get deployment/ecommerce-api -n ecommerce -o yaml > /tmp/backup-deployment.yaml
echo "현재 버전: $(kubectl get deployment/ecommerce-api -n ecommerce -o jsonpath='{.spec.template.spec.containers[0].image}')"

# Slack 알림: 배포 시작
# #deploy-alerts: "🚀 [프로덕션] ecommerce-api ${VERSION} 배포 시작 - @담당자"

# 이미지 업데이트
kubectl set image deployment/ecommerce-api \
  api=registry.example.com/ecommerce-api:${VERSION} \
  -n ecommerce

# 배포 상태 확인
kubectl rollout status deployment/ecommerce-api -n ecommerce --timeout=300s
```

---

## 6단계: 프로덕션 검증

```bash
# 헬스 체크
curl https://api.example.com/health

# 핵심 API 확인
curl https://api.example.com/api/products | head -c 200

# Pod 상태 확인
kubectl get pods -n ecommerce -l app=ecommerce-api

# 최근 로그 확인 (에러 없는지)
kubectl logs -n ecommerce -l app=ecommerce-api --tail=50 --since=5m
```

**체크리스트:**
- [ ] 모든 Pod이 Running 상태
- [ ] 헬스 체크 통과
- [ ] 최근 로그에 에러 없음
- [ ] Grafana 대시보드 이상 없음
- [ ] Sentry 신규 에러 없음

---

## 7단계: 배포 완료

```bash
# Slack 알림: 배포 완료
# #deploy-alerts: "✅ [프로덕션] ecommerce-api ${VERSION} 배포 완료"

# Git 태그
git tag -a ${VERSION} -m "Release ${VERSION}"
git push origin ${VERSION}
```

---

## 롤백이 필요한 경우

[롤백 절차](rollback-procedure.md)를 참고하세요.

```bash
# 긴급 롤백 (즉시 실행)
kubectl rollout undo deployment/ecommerce-api -n ecommerce
kubectl rollout status deployment/ecommerce-api -n ecommerce --timeout=300s
```
