# 로컬 제어기 작업 지침

- 한국어로 결론과 결과를 먼저 보고한다. 변경·검증·미검증 범위·위험을 기록한다.
- 이 프로젝트는 독립 Go 제어기다. 다른 프로젝트의 코드·설정·Git·런타임·인증을 변경하지 않는다.
- 명시적 사용자 요청과 task 범위만 권한 근거다. Notion 상태, 프로필, 설치된 도구는 새 권한을 발급하지 않는다.
- 정책은 `internal/core`의 동기식 순수 함수로 유지한다. 파일 확인은 `internal/config`, CLI는 `cmd/orchestrator-mcp`가 담당한다.
- CLI는 dry-run 전용이다. MTT-572 범위의 읽기 전용 Notion reader와 admission adapter·fake 검증만 포함한다. Reader의 고정 HTTPS GET은 별도 access 권한과 caller가 제공한 token을 요구하며, 실제 claim·task launch는 별도 권한 없이는 거부한다. 인증 파일 접근, provider executor, daemon, Docker/MCP 서비스, 범용 shell 기능을 추가하지 않는다.
- 프로필 없음, 불완전한 요청, 허용 목록 밖 task/권한, 다른 workspace는 기본 거부한다. consumer 이름이나 lifecycle을 core에 넣지 않는다.
- 표준 라이브러리를 먼저 사용한다. 변경에 필요한 호출부·fixture·테스트·문서를 함께 수정한다.
- 최종 검증은 `make check`다. task 고유 증거는 native CLI dry-run으로 보완한다. concurrency가 생기면 race 검증을 추가한다.
- 검증 산출물과 임시 파일은 `.local/`에 둔다. 전역 설치·설정 변경과 외부 의존성 다운로드를 하지 않는다.
- Git 초기화/branch/commit/push/PR, 실제 task launch, 외부 연결은 별도 권한이 필요하다. 이를 완료한 것처럼 보고하지 않는다.
- MTT managed task의 coordination은 해당 Operating Protocol을 따른다. 독립 리뷰 전에는 `READY_FOR_REVIEW`로 인계하며 `DONE`을 자체 선언하지 않는다.
