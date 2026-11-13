// Package loomy 实现 Loomy（讯飞桌面 AI 助手）上游协议：
// 账号服务登录（HMAC-SHA1 签名 + RSA/PKCS#1 v1.5 加密口令）、
// OpenAI 兼容模型网关（chat/completions、models、points）、
// 每日签到（/points/first-login）与新手任务（/onboarding/tasks）。
//
// 协议来自官方客户端逆向（参考 loomy2api / Loomy-Daily 两个开源实现）：
//   - 模型网关：https://loomyad.xunfei.cn/api/v1（OpenAI 兼容，session 即 Bearer）
//   - 账号服务：https://account.xfinfr.com（登录 / userinfo，需签名）
//   - 会话 14 天有效、无 refresh token，到期用手机号+密码重新登录即可续期。
package loomy

// 默认端点与客户端常量（均可在 config 覆盖）。
const (
	// DefaultModelBase 模型网关（OpenAI 兼容）。
	DefaultModelBase = "https://loomyad.xunfei.cn/api/v1"
	// DefaultAccountBase 账号服务（登录 / userinfo）。
	DefaultAccountBase = "https://account.xfinfr.com"
	// DefaultAccountAppID 账号服务应用标识。
	DefaultAccountAppID = "GM3LOOMY"

	// DefaultAccessKeyID/Secret 官方客户端 .env.prod 内置的账号服务签名密钥对。
	// 仅用于登录换取 session；模型网关本身不走签名。
	DefaultAccessKeyID     = "2thryby66wxi53sk"
	DefaultAccessKeySecret = "zsak6eadrbawz683wf5r3m2snrwj868r"

	// DefaultLoomyVersion 出站 loomy-version 头（上游仅校验存在）。
	DefaultLoomyVersion = "0.9.38"

	// DefaultDeviceID/WebModelID/ClientVersion/ClientUA 账号服务请求信封字段。
	DefaultDeviceID      = "web"
	DefaultWebModelID    = "Web"
	DefaultClientVersion = "1.0.0"
	DefaultClientUA      = "Loomy|Desktop|Electron|macOS"

	// CampusDeviceIDPrefix 推广设备号前缀（仅用于 /points/first-login 等权益接口）。
	CampusDeviceIDPrefix = "loomy-campus-"

	// SessionExpireSeconds 会话有效期：客户端固定申请 14 天。
	SessionExpireSeconds = 14 * 24 * 3600

	// SuccessCode 上游业务成功码。
	SuccessCode = "000000"

	// DefaultRequestPurpose 聊天请求归因（X-Loomy-Request-Purpose）。
	DefaultRequestPurpose = "chat.message"
)

// DefaultModel 上游默认模型。
const DefaultModel = "deepseek-v4-flash-0731"

// 路径常量。
const (
	PathGetPuKey   = "/login/account/getPuKey"
	PathLoginByPwd = "/login/account/byPwd"
	PathSendSMS    = "/login/phone/sendMsgCode"
	PathCheckSMS   = "/login/phone/checkCode"
	PathUserInfo   = "/userinfo/query/baseInfo"

	PathChat               = "/chat/completions"
	PathModels             = "/models"
	PathPoints             = "/points/records"
	PathFirstLogin         = "/points/first-login"
	PathOnboardingTasks    = "/onboarding/tasks"
	PathOnboardingComplete = "/onboarding/tasks/complete"
)

// OnboardingTask 一项新手任务（来自 Loomy-Daily 的 8 项任务表）。
type OnboardingTask struct {
	Group  string
	Key    string
	Title  string
	Points int
}

// OnboardingTasks 新手任务清单（共 8 项 / 10000 分）。
var OnboardingTasks = []OnboardingTask{
	{"初识 Loomy", "first_message", "发送你的第一条消息", 500},
	{"初识 Loomy", "pick_skill", "试试选择一个技能", 1000},
	{"初识 Loomy", "generate_ppt", "生成第一份 PPT", 1500},
	{"打造你的专属 Loomy", "set_schedule", "设置定时任务", 1000},
	{"打造你的专属 Loomy", "install_skill", "在技能广场安装一个技能", 1500},
	{"打造你的专属 Loomy", "configure_remote", "配置远程控制", 1000},
	{"遇见你的 AI 搭子", "create_soul", "创建你的第一个搭子", 1500},
	{"遇见你的 AI 搭子", "share_soul", "把搭子分享给朋友", 2000},
}

// OnboardingTotalPoints 新手任务总积分。
const OnboardingTotalPoints = 10000

// FallbackModel 内置兜底模型（上游 /models 拉取失败时使用）。
type FallbackModel struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	Multiplier    float64 `json:"multiplier,omitempty"`
	ContextLength int     `json:"context_length"`
}

// FallbackModels 兜底模型清单（对齐 loomy2api constants.FALLBACK_MODELS）。
var FallbackModels = []FallbackModel{
	{"spark-x", "Spark X2.5 (x0.1)", 0.1, 1048576},
	{"GLM-5.3-Flash", "GLM 5.3 Flash (x0.8)", 0.8, 1048576},
	{"qwen3.8-flash", "qwen 3.8 flash (x0.8)", 0.8, 1000000},
	{"deepseek-v4-flash-0731", "DeepSeek V4 Flash 0731 (x3.0)", 3.0, 1048576},
	{"mimo-v2.5", "MiMo V2.5 (x3.3)", 3.3, 1048576},
	{"MiniMax-M3", "MiniMax M3 (x4.0)", 4.0, 1048576},
	{"Kimi-k2.6", "Kimi k2.6 (x6.5)", 6.5, 262144},
	{"qwen-3.8-max", "Qwen 3.8 Max (x12.0)", 12.0, 1000000},
}
