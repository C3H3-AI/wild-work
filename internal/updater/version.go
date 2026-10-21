package updater

import (
	"strconv"
	"strings"
)

// CleanVersion 去除前导 'v' 或 'V'，去除多余空白字符。
func CleanVersion(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	v = strings.TrimPrefix(v, "V")
	return strings.TrimSpace(v)
}

// CompareVersions 比较两个版本号字符串（支持带或不带 'v' 前缀）。
// 返回值：
//   -1: v1 < v2
//    0: v1 == v2
//    1: v1 > v2
//
// 语义规则：
// 1. 去除 'v' 前缀。
// 2. 支持带有预发布后缀（如 "2.9.4-beta"），有后缀的版本低于同版本的无后缀版本。
// 3. 点分段按数值比较，如 "2.10.0" > "2.9.4"。
func CompareVersions(v1, v2 string) int {
	s1 := CleanVersion(v1)
	s2 := CleanVersion(v2)
	if s1 == s2 {
		return 0
	}
	if s1 == "" && s2 == "" {
		return 0
	}
	if s1 == "" {
		return -1
	}
	if s2 == "" {
		return 1
	}

	// 拆分主版本段与预发布段（如 2.9.4-rc1）
	core1, pre1 := splitPreRelease(s1)
	core2, pre2 := splitPreRelease(s2)

	parts1 := strings.Split(core1, ".")
	parts2 := strings.Split(core2, ".")

	maxLen := len(parts1)
	if len(parts2) > maxLen {
		maxLen = len(parts2)
	}

	for i := 0; i < maxLen; i++ {
		var n1, n2 int
		if i < len(parts1) {
			n1, _ = strconv.Atoi(parts1[i])
		}
		if i < len(parts2) {
			n2, _ = strconv.Atoi(parts2[i])
		}
		if n1 < n2 {
			return -1
		}
		if n1 > n2 {
			return 1
		}
	}

	// 主版本相同的情况下，判断预发布标识
	// 无预发布后缀的正式版 > 带预发布后缀的测试版
	if pre1 == "" && pre2 != "" {
		return 1
	}
	if pre1 != "" && pre2 == "" {
		return -1
	}
	if pre1 < pre2 {
		return -1
	}
	if pre1 > pre2 {
		return 1
	}

	return 0
}

func splitPreRelease(s string) (core, pre string) {
	if idx := strings.Index(s, "-"); idx >= 0 {
		return s[:idx], s[idx+1:]
	}
	return s, ""
}
