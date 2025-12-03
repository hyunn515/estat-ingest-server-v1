# syntax=docker/dockerfile:1.7

##############################
# 1. Builder
##############################
FROM golang:1.22-bookworm AS builder

# 모듈 캐시를 재사용하기 위해 먼저 go.mod/go.sum만 복사
WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

# 나머지 소스 전체 복사
COPY . .

# 빌드 타겟 (ECS/Fargate는 linux/amd64 또는 linux/arm64)
ARG TARGETOS=linux
ARG TARGETARCH=amd64

# CGO 끄고 정적에 가깝게 빌드 (distroless에서 잘 동작)
ENV CGO_ENABLED=0 \
    GOOS=${TARGETOS} \
    GOARCH=${TARGETARCH}

# 단일 바이너리 빌드
# -trimpath : 빌드 경로 제거
# -s -w     : 심볼/디버그 정보 제거 (바이너리 작게)
RUN go build -trimpath -ldflags="-s -w" -o /out/estat-server ./cmd/server

##############################
# 2. Runtime (distroless)
##############################
FROM gcr.io/distroless/base-debian12 AS runtime

# 애플리케이션 작업 디렉토리
WORKDIR /app

# 빌더에서 빌드한 바이너리만 복사
COPY --from=builder /out/estat-server /app/estat-server

# 비루트 유저로 실행 (distroless의 기본 nonroot)
USER nonroot:nonroot

# 애플리케이션 포트 (HTTP_ADDR=:8080 가정)
EXPOSE 8080

# 엔트리포인트
ENTRYPOINT ["/app/estat-server"]