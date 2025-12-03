package server

import (
	"net"
	"net/http"
	"strings"
)

// ------------------------------------------------------------
// IP Utility Functions
//
// ingest 서버는 ALB 또는 CloudFront 뒤에 배치되므로
// RemoteAddr 만으로는 "실제 사용자 IP"를 알 수 없다.
// 아래 로직은 AWS 표준 헤더 기반으로
// 가장 신뢰할 수 있는 클라이언트 IP를 추출한다.
// ------------------------------------------------------------

// isPublicIP:
//   - private / loopback / link-local 등이 아닌 경우 true
//   - X-Forwarded-For에서 내부 IP를 제외하기 위해 필요
func isPublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	// IPv4 private ranges: 10/8, 172.16/12, 192.168/16
	if ip.IsPrivate() {
		return false
	}
	// Loopback, link-local 등 제외
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false
	}
	return true
}

// safeParseIP:
//   - 공백/빈 값 대응
//   - 잘못된 값이 들어오면 nil 반환
func safeParseIP(s string) net.IP {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return net.ParseIP(s)
}

// ------------------------------------------------------------
// clientIP:
//
// "실제 사용자(브라우저)의 IP"를 추출.
// 우선순위:
//  1. X-Forwarded-For → 첫 번째 public IP
//  2. CloudFront-Viewer-Address → 포트 제거 후 IP 사용
//  3. RemoteAddr fallback
//
// ALB 뒤에 있는 ECS 환경에서 가장 표준적인 구현.
// ------------------------------------------------------------
func clientIP(r *http.Request) string {

	// 1) X-Forwarded-For (ALB)
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// 예: "203.0.113.1, 10.0.1.24"
		parts := strings.Split(xff, ",")
		for _, part := range parts {
			ip := safeParseIP(part)
			if isPublicIP(ip) {
				return ip.String()
			}
		}
	}

	// 2) CloudFront-Viewer-Address
	// 예: "203.0.113.55:44321" 또는 "2404:6800:4004::200e:44321"
	if cf := r.Header.Get("CloudFront-Viewer-Address"); cf != "" {
		host := cf
		// 마지막 ":" 를 기준으로 포트 제거 (IPv6 포함 대응)
		if i := strings.LastIndex(cf, ":"); i != -1 {
			host = cf[:i]
		}
		ip := safeParseIP(host)
		if isPublicIP(ip) {
			return ip.String()
		}
	}

	// 3) RemoteAddr fallback
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		ip := safeParseIP(host)
		if isPublicIP(ip) {
			return ip.String()
		}
	}

	return ""
}
