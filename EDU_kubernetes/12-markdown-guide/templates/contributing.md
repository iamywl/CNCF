# 기여 가이드 (Contributing Guide)

이 프로젝트에 기여해 주셔서 감사합니다!
아래 가이드를 따라주시면 원활한 협업이 가능합니다.

## 기여 방법

### 1. 이슈 확인
- 작업 전 [Issues](https://github.com/username/project/issues)에서 관련 이슈를 확인하세요.
- 새로운 기능이나 버그 리포트는 이슈를 먼저 생성해 주세요.

### 2. Fork & Clone
```bash
git clone https://github.com/your-username/project.git
cd project
git remote add upstream https://github.com/username/project.git
```

### 3. 브랜치 생성
```bash
# 기능 추가
git checkout -b feature/기능명

# 버그 수정
git checkout -b fix/버그설명

# 문서 수정
git checkout -b docs/문서설명
```

### 4. 개발 & 테스트
```bash
# 의존성 설치
npm install

# 테스트 실행
npm test

# 린트 검사
npm run lint
```

### 5. 커밋
[Conventional Commits](https://www.conventionalcommits.org/) 형식을 따릅니다:

```
<type>(<scope>): <description>

feat(auth): 소셜 로그인 기능 추가
fix(cart): 수량 음수 입력 가능한 버그 수정
docs(readme): 설치 방법 업데이트
refactor(api): 에러 핸들링 미들웨어 분리
test(order): 주문 생성 유닛 테스트 추가
```

| type | 설명 |
|------|------|
| `feat` | 새로운 기능 |
| `fix` | 버그 수정 |
| `docs` | 문서 변경 |
| `style` | 코드 포맷팅 (기능 변경 없음) |
| `refactor` | 리팩토링 |
| `test` | 테스트 추가/수정 |
| `chore` | 빌드, 설정 변경 |

### 6. Pull Request
```bash
git push origin feature/기능명
```
- GitHub에서 Pull Request를 생성합니다.
- PR 템플릿을 채워주세요.
- 최소 1명의 리뷰어 승인이 필요합니다.

## 코드 스타일

- ESLint + Prettier 설정을 따릅니다.
- `npm run lint`가 통과해야 PR이 머지됩니다.
- 함수명: camelCase
- 파일명: kebab-case
- 컴포넌트: PascalCase

## 리뷰 기준

리뷰어는 아래 항목을 확인합니다:
- [ ] 코드가 의도대로 동작하는가
- [ ] 테스트가 추가/수정되었는가
- [ ] 기존 테스트가 모두 통과하는가
- [ ] 코드 스타일 가이드를 따르는가
- [ ] 문서가 업데이트되었는가 (필요한 경우)

## 질문이 있으시면

- GitHub Issues에 `question` 라벨로 이슈를 생성해 주세요.
- 이메일: team@example.com
