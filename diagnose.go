package main

import "regexp"

// ===== 启动失败智能诊断 =====

type diagRule struct {
	re     *regexp.Regexp
	reason string
	advice string
}

var diagRules = []diagRule{
	{
		regexp.MustCompile(`(?i)FAILED TO BIND TO PORT|Address already in use|bind.*:(\d+)`),
		"端口被占用",
		"这个端口已被其他程序（或另一个世界）占用。改一个端口，或先停掉占用端口的程序。",
	},
	{
		regexp.MustCompile(`(?i)OutOfMemoryError|Out of memory|GC overhead limit`),
		"内存不足",
		"分配给服务器的内存不够。在设置里调大最大内存，或减少模组数量。",
	},
	{
		regexp.MustCompile(`(?i)UnsupportedClassVersionError|class file version (\d+)`),
		"Java 版本不匹配",
		"服务端需要更高版本的 Java。删除实例设置里的自定义 Java 路径，面板会自动装对应版本。",
	},
	{
		regexp.MustCompile(`(?i)You need to agree to the EULA`),
		"未同意 EULA",
		"eula.txt 未签署。面板通常会自动处理；在文件管理里把 eula.txt 内容改为 eula=true。",
	},
	{
		regexp.MustCompile(`(?i)Failed to load level|Exception reading .*level\.dat|ChunkIOError|corrupt(ed)? (chunk|world|region|level)`),
		"存档可能损坏",
		"世界存档读取失败。到「备份」页恢复最近的备份，或删除 world 文件夹重新生成世界。",
	},
	{
		regexp.MustCompile(`(?i)Missing or unsupported mandatory dependencies|requires .* of mod|DuplicateModsFoundException|Incompatible mods found`),
		"模组缺依赖或冲突",
		"某个模组缺少前置或版本冲突。查看控制台里提到的模组名，补装前置或移除冲突模组。",
	},
	{
		regexp.MustCompile(`(?i)Unable to launch|Invalid or corrupt jarfile|Error: Could not find or load main class`),
		"服务端核心文件损坏",
		"核心 jar 可能没下载完整。到服务器配置页重新下载/更新核心。",
	},
	{
		regexp.MustCompile(`(?i)java\.net\.UnknownHostException|Connection timed out.*authserver|session server`),
		"网络连接问题",
		"服务器无法连接验证/资源服务器。检查电脑网络或代理设置，离线玩不影响启动。",
	},
}

// diagnose returns a friendly reason+advice for a console line, or "" if no match.
func diagnose(line string) string {
	for _, r := range diagRules {
		if r.re.MatchString(line) {
			return r.reason + " — " + r.advice
		}
	}
	return ""
}
