// config.go 面板的配置读写：读原始 config.json、深合并写回、真实语义校验。
//
// 关于「热生效」的诚实说明：网关在启动时一次性装配 upstream client、pool 参数、
// scheduler、session router 与 handler，因此**几乎所有配置项都需要重启才能生效**。
// 面板的做法是：原子写盘 + 一键重启（依赖 docker restart 策略秒级拉起），
// 而不是假装热生效。唯一例外见 RestartRequiredKeys。
package panel

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// RestartRequiredKeys 需要重启生效的配置段（全部，见包注释）。保留此清单是为了
// 前端能明确提示，而不是让用户误以为保存即生效。
var RestartRequiredKeys = []string{
	"listen", "api_key", "auth_dir", "state_file",
	"server", "cooldown", "schedule", "global", "upstream",
	"features", "prompt", "upstash", "pool", "session_sticky",
}

// handleConfigGet 返回 config.json 原文（解析为 JSON 对象，保留未知键）。
func (p *Panel) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	raw, err := os.ReadFile(p.deps.ConfigPath)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "读取配置失败: "+err.Error())
		return
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		errJSON(w, http.StatusInternalServerError, "解析配置失败: "+err.Error())
		return
	}
	st, _ := os.Stat(p.deps.ConfigPath)
	okJSON(w, map[string]any{
		"ok":            true,
		"config":        obj,
		"path":          p.deps.ConfigPath,
		"restart_keys":  RestartRequiredKeys,
		"modified_at":   modTime(st),
		"raw_size":      len(raw),
	})
}

// handleConfigPut 接收一份**增量**配置（只传要改的段即可），深合并进现有 config.json，
// 用网关真实的校验收口后原子落盘。
//
// 请求体：{"config": { ... 增量 ... }}
func (p *Panel) handleConfigPut(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Config map[string]any `json:"config"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&body); err != nil {
		errJSON(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	if body.Config == nil {
		errJSON(w, http.StatusBadRequest, "缺少 config 字段")
		return
	}

	// 1) 读现有配置（保留未知键，避免面板把用户手写的扩展字段洗掉）
	curRaw, err := os.ReadFile(p.deps.ConfigPath)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "读取现有配置失败: "+err.Error())
		return
	}
	var cur map[string]any
	if err := json.Unmarshal(curRaw, &cur); err != nil {
		errJSON(w, http.StatusInternalServerError, "现有配置不是合法 JSON: "+err.Error())
		return
	}

	// 2) 深合并
	merged := deepMerge(cur, body.Config)

	// 3) 用真实语义校验（dry-run）：类型错误、非法 duration、非法 prompt.mode 等在此拦下
	out, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "序列化失败: "+err.Error())
		return
	}
	if p.deps.ValidateConfig != nil {
		if err := p.deps.ValidateConfig(out); err != nil {
			errJSON(w, http.StatusBadRequest, "配置校验失败: "+err.Error())
			return
		}
	}

	// 4) 原子落盘（同目录临时文件 + rename），保留原文件权限
	mode := os.FileMode(0o600)
	if st, err := os.Stat(p.deps.ConfigPath); err == nil {
		mode = st.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(p.deps.ConfigPath), ".config-*.tmp")
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "创建临时文件失败: "+err.Error())
		return
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // rename 成功后此调用无害
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		errJSON(w, http.StatusInternalServerError, "写入失败: "+err.Error())
		return
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		errJSON(w, http.StatusInternalServerError, "设置权限失败: "+err.Error())
		return
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		errJSON(w, http.StatusInternalServerError, "刷盘失败: "+err.Error())
		return
	}
	tmp.Close()
	if err := os.Rename(tmpName, p.deps.ConfigPath); err != nil {
		// 单文件 bind mount 场景（docker 把宿主机 config.json 挂进容器）下，rename 覆盖
		// 挂载点必然失败：Linux 报 EBUSY（device or resource busy），部分平台报 EXDEV。
		// 这里对**任何** rename 失败都退化为原地覆盖写——两个都失败时才如实上报，
		// 且同时给出两个错误（rename 的错通常更能说明根因，如只读文件系统）。
		if werr := os.WriteFile(p.deps.ConfigPath, out, mode); werr != nil {
			errJSON(w, http.StatusInternalServerError,
				"写入配置失败: rename: "+err.Error()+"；原地写: "+werr.Error())
			return
		}
		log.Printf("[panel] config.json rename 失败（%v），已退化为原地覆盖写", err)
	}

	okJSON(w, map[string]any{
		"ok":             true,
		"msg":            "配置已保存，需重启网关后生效",
		"restart_needed": true,
		"config":         merged,
	})
}

// deepMerge 把 patch 递归合并进 base（返回新 map，不修改入参）。
//
// 语义：
//   - 两边同为 object → 递归合并
//   - 其余情况（含数组、标量、类型不一致）→ patch 整体覆盖 base
//   - patch 中显式 null → 覆盖为 null（用于清空字段）
func deepMerge(base, patch map[string]any) map[string]any {
	out := make(map[string]any, len(base))
	for k, v := range base {
		out[k] = v
	}
	for k, pv := range patch {
		if bv, ok := out[k]; ok {
			bm, bok := bv.(map[string]any)
			pm, pok := pv.(map[string]any)
			if bok && pok {
				out[k] = deepMerge(bm, pm)
				continue
			}
		}
		out[k] = pv
	}
	return out
}

func modTime(st os.FileInfo) string {
	if st == nil {
		return ""
	}
	return st.ModTime().Format(time.RFC3339)
}

// prettyJSON 仅用于调试输出（保留备用，避免 bytes 导入被优化掉）。
func prettyJSON(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
	return b.String()
}
