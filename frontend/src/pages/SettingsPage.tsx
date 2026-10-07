import { useCallback, useEffect, useState } from "react"
import { Settings2, RefreshCw, KeyRound, ServerCrash, Code, Activity, Save } from "lucide-react"
import { Button } from "../components/ui/button"
import { toast } from "sonner"
import { useTranslation } from "react-i18next"
import { adminRequestErrorMessage, clearStoredApiKey, getAuthHeader, getStoredApiKey, setStoredApiKey } from "../lib/auth"
import { API_BASE } from "../lib/api"
import {
  capabilityBadges,
  fetchModelOptions,
  formatModeLabel,
  formatModelName,
  groupModelOptions,
  type ModelOption,
} from "../lib/models"

type ModelAliases = Record<string, string>

interface AdminSettings {
  version?: string
  max_inflight_per_account?: number
  global_max_inflight?: number
  chat_id_pool_target?: number
  chat_id_pool_ttl_seconds?: number
  keepalive_url?: string
  keepalive_interval?: number
  keepalive_env_locked?: string[]
  keepalive_running?: boolean
  model_aliases?: ModelAliases
}

export default function SettingsPage() {
  const { t } = useTranslation()
  const [settings, setSettings] = useState<AdminSettings | null>(null)
  const [sessionKey, setSessionKey] = useState(() => getStoredApiKey())
  const [maxInflight, setMaxInflight] = useState(4)
  const [globalMaxInflight, setGlobalMaxInflight] = useState(0)
  const [poolTarget, setPoolTarget] = useState(0)
  const [poolTtlMin, setPoolTtlMin] = useState(10)
  const [keepaliveUrl, setKeepaliveUrl] = useState("")
  const [keepaliveInterval, setKeepaliveInterval] = useState(60)
  const [keepaliveEnvLocked, setKeepaliveEnvLocked] = useState<string[]>([])
  const [keepaliveRunning, setKeepaliveRunning] = useState(false)
  const [modelAliases, setModelAliases] = useState("")
  const [models, setModels] = useState<ModelOption[]>([])
  const [modelsLoading, setModelsLoading] = useState(true)

  const fetchSettings = useCallback(() => {
    if (!getStoredApiKey()) {
      toast.error(t("settings.pasteKey"))
      return
    }
    fetch(`${API_BASE}/api/admin/settings`, { headers: getAuthHeader() })
      .then(async res => {
        if(!res.ok) throw new Error(await adminRequestErrorMessage(res))
        return res.json()
      })
      .then(data => {
        setSettings(data)
        setMaxInflight(data.max_inflight_per_account || 4)
        setGlobalMaxInflight(data.global_max_inflight || 0)
        setPoolTarget(data.chat_id_pool_target ?? 0)
        setPoolTtlMin(Math.round((data.chat_id_pool_ttl_seconds || 600) / 60))
        setKeepaliveUrl(data.keepalive_url || "")
        setKeepaliveInterval(data.keepalive_interval || 60)
        setKeepaliveEnvLocked(data.keepalive_env_locked || [])
        setKeepaliveRunning(Boolean(data.keepalive_running))
        setModelAliases(JSON.stringify(data.model_aliases || {}, null, 2))
      })
      .catch(err => toast.error(err instanceof Error ? err.message : t("settings.fetchFailed")))
  }, [t])

  const loadModels = useCallback(() => {
    fetchModelOptions()
      .then(setModels)
      .catch(() => setModels([]))
      .finally(() => setModelsLoading(false))
  }, [])

  const fetchModels = useCallback(() => {
    setModelsLoading(true)
    loadModels()
  }, [loadModels])

  useEffect(() => {
    fetchSettings()
    fetchModels()
  }, [fetchSettings, fetchModels])

  const handleSaveSessionKey = () => {
    const key = setStoredApiKey(sessionKey)
    if (!key) {
      toast.error(t("settings.enterKey"))
      return
    }
    setSessionKey(key)
    toast.success(t("settings.keySaved"))
    fetchSettings()
  }

  const handleClearSessionKey = () => {
    clearStoredApiKey()
    setSessionKey("")
    toast.success(t("settings.keyCleared"))
  }

  const handleSaveConcurrency = () => {
    fetch(`${API_BASE}/api/admin/settings`, {
      method: "PUT",
      headers: { "Content-Type": "application/json", ...getAuthHeader() },
      body: JSON.stringify({
        max_inflight_per_account: Number(maxInflight),
        global_max_inflight: Number(globalMaxInflight),
      })
    }).then(res => {
      if(res.ok) { toast.success(t("settings.concurrencySaved")); fetchSettings(); }
      else toast.error(t("settings.saveFailed"))
    })
  }

  const handleSavePool = () => {
    fetch(`${API_BASE}/api/admin/settings`, {
      method: "PUT",
      headers: { "Content-Type": "application/json", ...getAuthHeader() },
      body: JSON.stringify({
        chat_id_pool_target: Number(poolTarget),
        chat_id_pool_ttl_seconds: Number(poolTtlMin) * 60,
      })
    }).then(res => {
      if(res.ok) { toast.success(t("settings.poolSaved")); fetchSettings(); }
      else toast.error(t("settings.saveFailed"))
    })
  }

  const handleUseCurrentKeepaliveUrl = () => {
    setKeepaliveUrl(`${baseUrl.replace(/\/$/, "")}/keepalive`)
  }

  const handleSaveKeepalive = () => {
    const interval = Number(keepaliveInterval)
    if (!Number.isFinite(interval) || interval < 5 || interval > 86400) {
      toast.error(t("settings.keepaliveIntervalRange"))
      return
    }

    fetch(`${API_BASE}/api/admin/settings`, {
      method: "PUT",
      headers: { "Content-Type": "application/json", ...getAuthHeader() },
      body: JSON.stringify({
        keepalive_url: keepaliveUrl.trim(),
        keepalive_interval: interval,
      })
    }).then(async res => {
      const data = await res.json().catch(() => ({}))
      if(res.ok) { toast.success(t("settings.keepaliveSaved")); fetchSettings(); }
      else toast.error(data.detail || t("settings.saveFailed"))
    }).catch(() => toast.error(t("settings.saveFailed")))
  }

  const handleSaveAliases = () => {
    try {
      const parsed = JSON.parse(modelAliases)
      fetch(`${API_BASE}/api/admin/settings`, {
        method: "PUT",
        headers: { "Content-Type": "application/json", ...getAuthHeader() },
        body: JSON.stringify({ model_aliases: parsed })
      }).then(res => {
        if(res.ok) { toast.success(t("settings.aliasesSaved")); fetchSettings(); }
        else toast.error(t("settings.saveFailed"))
      })
    } catch {
      toast.error(t("settings.jsonError"))
    }
  }

  const baseUrl = API_BASE || `http://${window.location.hostname}:7860`
  const modelGroups = groupModelOptions(models)

  const curlExample = `# OpenAI streaming chat
  curl ${baseUrl}/v1/chat/completions \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer YOUR_API_KEY" \
    -d '{
      "model": "qwen3.6-plus",
      "messages": [{"role": "user", "content": "Hello"}],
      "stream": true
    }'

  # Anthropic / Claude Code
  curl ${baseUrl}/anthropic/v1/messages \
    -H "Content-Type: application/json" \
    -H "x-api-key: YOUR_API_KEY" \
    -H "anthropic-version: 2023-06-01" \
    -d '{
      "model": "claude-sonnet-4-6",
      "max_tokens": 1024,
      "messages": [{"role": "user", "content": "Hello"}]
    }'

  # Gemini
  curl ${baseUrl}/v1beta/models/qwen3.6-plus:generateContent \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer YOUR_API_KEY" \
    -d '{
      "contents": [{"parts": [{"text": "Hello"}]}]
    }'

  # Images
  curl ${baseUrl}/v1/images/generations \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer YOUR_API_KEY" \
    -d '{
      "model": "qwen3.6-plus-image",
      "prompt": "A cyberpunk cat with neon lights, ultra realistic",
      "n": 1,
      "size": "1328x1328",
      "response_format": "url"
    }'

  # Video
  curl ${baseUrl}/v1/videos/generations \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer YOUR_API_KEY" \
    -d '{
      "model": "qwen3.6-plus-video",
      "prompt": "Generate a slow-motion ocean-wave video.",
      "duration": 5,
      "size": "1664x928",
      "ratio": "16:9",
      "response_format": "url"
    }'`

  return (
    <div className="w-full min-w-0 overflow-x-hidden space-y-6">
      <section className="admin-hero p-6">
        <div className="relative z-10 flex justify-between items-end flex-wrap gap-4">
          <div className="min-w-0">
            <div className="text-xs font-black uppercase tracking-[0.28em] text-muted-foreground">Control Plane</div>
            <h2 className="mt-2 text-4xl font-black tracking-tight">{t("settings.title")}</h2>
            <p className="mt-2 text-muted-foreground">{t("settings.desc")}</p>
          </div>
        <Button variant="outline" onClick={() => {fetchSettings(); fetchModels(); toast.success(t("settings.refreshed"))}}>
          <RefreshCw className="mr-2 h-4 w-4" /> {t("settings.refresh")}
        </Button>
        </div>
      </section>

      <div className="grid gap-6 min-w-0">
        {/* Session Key */}
        <div className="admin-card min-w-0 overflow-hidden">
          <div className="admin-card-header flex flex-col space-y-1.5">
            <div className="flex items-center gap-2">
              <KeyRound className="h-5 w-5 text-primary" />
              <h3 className="font-semibold leading-none tracking-tight">{t("settings.sessionKey")}</h3>
            </div>
            <p className="text-sm text-muted-foreground">{t("settings.sessionKeyDesc")}</p>
          </div>
          <div className="p-6">
            <div className="flex gap-2 items-center flex-wrap">
              <input
                type="password"
                value={sessionKey}
                onChange={e => setSessionKey(e.target.value)}
                placeholder={t("settings.sessionKeyPlaceholder")}
                className="admin-input flex h-10 flex-1 min-w-[200px] px-3 py-2 text-sm"
              />
              <Button onClick={handleSaveSessionKey}>{t("settings.save")}</Button>
              <Button variant="ghost" onClick={handleClearSessionKey}>{t("settings.clear")}</Button>
            </div>
          </div>
        </div>

        {/* Connection Info */}
        <div className="admin-card min-w-0 overflow-hidden">
          <div className="admin-card-header flex flex-col space-y-1.5">
            <div className="flex items-center gap-2">
              <ServerCrash className="h-5 w-5 text-primary" />
              <h3 className="font-semibold leading-none tracking-tight">{t("settings.connection")}</h3>
            </div>
          </div>
          <div className="p-6">
            <div className="space-y-1 min-w-0">
              <label className="text-sm font-medium">{t("settings.baseUrl")}</label>
              <input type="text" readOnly value={baseUrl} className="admin-input flex h-10 w-full px-3 py-2 text-sm font-mono text-muted-foreground" />
            </div>
          </div>
        </div>

        {/* Model Catalog */}
        <div className="admin-card min-w-0 overflow-hidden">
          <div className="admin-card-header flex flex-col space-y-1.5">
            <div className="flex items-center gap-2">
              <Settings2 className="h-5 w-5 text-primary" />
              <h3 className="font-semibold leading-none tracking-tight">{t("settings.catalog")}</h3>
            </div>
            <p className="text-sm text-muted-foreground">{t("settings.catalogDesc")}</p>
          </div>
          <div className="p-6 space-y-3">
            {modelsLoading ? (
              <div className="flex items-center gap-2 text-sm text-muted-foreground">
                <RefreshCw className="h-4 w-4 animate-spin" /> {t("settings.loadingModels")}
              </div>
            ) : modelGroups.length === 0 ? (
              <div className="rounded-lg border border-dashed bg-muted/20 p-4 text-sm text-muted-foreground">
                {t("settings.noModels")}
              </div>
            ) : (
              modelGroups.map((group, index) => (
                <details key={group.family} open={index === 0} className="rounded-lg border bg-background/60">
                  <summary className="cursor-pointer select-none px-4 py-3 text-sm font-semibold">
                    {group.family}
                    <span className="ml-2 rounded-full bg-muted px-2 py-0.5 text-xs font-normal text-muted-foreground">
                      {t("settings.modelCount", { count: group.models.length })}
                    </span>
                  </summary>
                  <div className="border-t divide-y">
                    {group.models.map(option => {
                      const badges = capabilityBadges(option)
                      return (
                        <div key={option.id} className="grid gap-2 px-4 py-3 text-sm md:grid-cols-[1.4fr_1fr_0.7fr_1fr] md:items-center">
                          <div className="min-w-0">
                            <div className="truncate font-medium">{formatModelName(option)}</div>
                            <div className="truncate font-mono text-xs text-muted-foreground">{option.id}</div>
                          </div>
                          <div className="min-w-0 font-mono text-xs text-muted-foreground">
                            base: {option.base_model || option.id}
                          </div>
                          <div>
                            <span className="rounded-full border bg-muted/50 px-2 py-0.5 text-xs">
                              {formatModeLabel(option.mode)}
                            </span>
                          </div>
                          <div className="flex flex-wrap gap-1">
                            {badges.length ? badges.map(label => (
                              <span key={label} className="rounded-full border border-primary/30 bg-primary/10 px-2 py-0.5 text-xs text-primary">
                                {label}
                              </span>
                            )) : (
                              <span className="text-xs text-muted-foreground">{t("settings.chatMode")}</span>
                            )}
                          </div>
                        </div>
                      )
                    })}
                  </div>
                </details>
              ))
            )}
          </div>
        </div>

        {/* Core Settings */}
        <div className="admin-card min-w-0 overflow-hidden">
          <div className="admin-card-header flex flex-col space-y-1.5">
            <div className="flex items-center gap-2">
              <Settings2 className="h-5 w-5 text-primary" />
              <h3 className="font-semibold leading-none tracking-tight">{t("settings.concurrency")}</h3>
            </div>
            <p className="text-sm text-muted-foreground">{t("settings.concurrencyDesc")}</p>
          </div>
          <div className="p-6 space-y-4">
            <div className="flex justify-between items-center py-2 border-b flex-wrap gap-2">
              <div className="space-y-1 min-w-0">
                <span className="text-sm font-medium">{t("settings.version")}</span>
              </div>
              <span className="font-mono text-sm">{settings?.version || "..."}</span>
            </div>
            <div className="flex justify-between items-center py-2 border-b flex-wrap gap-4">
              <div className="space-y-1 min-w-0 flex-1">
                <span className="text-sm font-medium">{t("settings.maxInflight")}</span>
                <p className="text-xs text-muted-foreground">{t("settings.maxInflightDesc")}</p>
              </div>
              <input
                type="number"
                min="1"
                max="10"
                value={maxInflight}
                onChange={e => setMaxInflight(Number(e.target.value))}
                className="admin-input flex h-8 w-20 px-3 py-1 text-sm text-center"
              />
            </div>
            <div className="flex justify-between items-center py-2 border-b flex-wrap gap-4">
              <div className="space-y-1 min-w-0 flex-1">
                <span className="text-sm font-medium">{t("settings.globalMax")}</span>
                <p className="text-xs text-muted-foreground">{t("settings.globalMaxDesc")}</p>
              </div>
              <input
                type="number"
                min="0"
                max="200"
                value={globalMaxInflight}
                onChange={e => setGlobalMaxInflight(Number(e.target.value))}
                className="admin-input flex h-8 w-20 px-3 py-1 text-sm text-center"
              />
            </div>
            <div className="flex justify-end">
              <Button size="sm" onClick={handleSaveConcurrency}>{t("settings.saveConcurrency")}</Button>
            </div>
          </div>
        </div>

        {/* Chat ID Pool */}
        <div className="admin-card min-w-0 overflow-hidden">
          <div className="admin-card-header flex flex-col space-y-1.5">
            <div className="flex items-center gap-2">
              <Settings2 className="h-5 w-5 text-rose-500" />
              <h3 className="font-semibold leading-none tracking-tight">{t("settings.pool")}</h3>
            </div>
            <p className="text-sm text-muted-foreground">{t("settings.poolDesc")}</p>
          </div>
          <div className="p-6 space-y-4">
            <div className="flex justify-between items-center py-2 border-b flex-wrap gap-4">
              <div className="space-y-1 min-w-0 flex-1">
                <span className="text-sm font-medium">{t("settings.poolTarget")}</span>
                <p className="text-xs text-muted-foreground">{t("settings.poolTargetDesc")}</p>
              </div>
              <input
                type="number"
                min="0"
                max="20"
                value={poolTarget}
                onChange={e => setPoolTarget(Number(e.target.value))}
                className="admin-input flex h-8 w-20 px-3 py-1 text-sm text-center"
              />
            </div>
            <div className="flex justify-between items-center py-2 border-b flex-wrap gap-4">
              <div className="space-y-1 min-w-0 flex-1">
                <span className="text-sm font-medium">{t("settings.poolTtl")}</span>
                <p className="text-xs text-muted-foreground">{t("settings.poolTtlDesc")}</p>
              </div>
              <input
                type="number"
                min="1"
                max="120"
                value={poolTtlMin}
                onChange={e => setPoolTtlMin(Number(e.target.value))}
                className="admin-input flex h-8 w-20 px-3 py-1 text-sm text-center"
              />
            </div>
            <div className="flex justify-end">
              <Button size="sm" onClick={handleSavePool}>{t("settings.savePool")}</Button>
            </div>
          </div>
        </div>

        {/* Keepalive */}
        <div className="rounded-xl border bg-card text-card-foreground shadow-sm min-w-0">
          <div className="flex flex-col space-y-1.5 p-6 border-b bg-muted/30">
            <div className="flex items-center gap-2">
              <Activity className="h-5 w-5 text-emerald-500" />
              <h3 className="font-semibold leading-none tracking-tight">{t("settings.keepalive")}</h3>
              <span className={`rounded-full px-2 py-0.5 text-xs font-medium ${keepaliveRunning ? "bg-emerald-500/10 text-emerald-600 dark:text-emerald-300" : "bg-muted text-muted-foreground"}`}>
                {keepaliveRunning ? t("settings.keepaliveRunning") : t("settings.keepaliveOff")}
              </span>
            </div>
            <p className="text-sm text-muted-foreground">{t("settings.keepaliveDesc")}</p>
          </div>
          <div className="p-6 space-y-4">
            <div className="space-y-2">
              <div className="flex items-center justify-between gap-3 flex-wrap">
                <label className="text-sm font-medium">{t("settings.keepaliveUrl")}</label>
                <Button variant="outline" size="sm" onClick={handleUseCurrentKeepaliveUrl} disabled={keepaliveEnvLocked.includes("keepalive_url")}>
                  <Activity className="mr-2 h-4 w-4" /> {t("settings.keepaliveQuick")}
                </Button>
              </div>
              <input
                type="text"
                value={keepaliveUrl}
                disabled={keepaliveEnvLocked.includes("keepalive_url")}
                onChange={e => setKeepaliveUrl(e.target.value)}
                placeholder={`${baseUrl.replace(/\/$/, "")}/keepalive`}
                className="flex h-10 w-full rounded-md border border-input bg-background px-3 py-2 text-sm disabled:cursor-not-allowed disabled:bg-muted"
              />
              {keepaliveEnvLocked.includes("keepalive_url") && (
                <p className="text-xs text-muted-foreground">{t("settings.keepaliveUrlLocked")}</p>
              )}
            </div>
            <div className="flex justify-between items-center py-2 border-b flex-wrap gap-4">
              <div className="space-y-1 min-w-0 flex-1">
                <span className="text-sm font-medium">{t("settings.keepaliveInterval")}</span>
                <p className="text-xs text-muted-foreground">{t("settings.keepaliveIntervalDesc")}</p>
              </div>
              <input
                type="number"
                min="5"
                max="86400"
                value={keepaliveInterval}
                disabled={keepaliveEnvLocked.includes("keepalive_interval")}
                onChange={e => setKeepaliveInterval(Number(e.target.value))}
                className="flex h-8 w-28 rounded-md border border-input bg-background px-3 py-1 text-sm text-center disabled:cursor-not-allowed disabled:bg-muted"
              />
            </div>
            {keepaliveEnvLocked.includes("keepalive_interval") && (
              <p className="text-xs text-muted-foreground">{t("settings.keepaliveIntervalLocked")}</p>
            )}
            <div className="flex justify-end">
              <Button size="sm" onClick={handleSaveKeepalive}>
                <Save className="mr-2 h-4 w-4" /> {t("settings.saveKeepalive")}
              </Button>
            </div>
          </div>
        </div>

        {/* Model Mapping */}
        <div className="admin-card min-w-0 overflow-hidden">
          <div className="admin-card-header flex flex-col space-y-1.5">
            <h3 className="font-semibold leading-none tracking-tight">{t("settings.aliases")}</h3>
            <p className="text-sm text-muted-foreground">{t("settings.aliasesDesc")}</p>
          </div>
          <div className="p-6">
            <textarea
              rows={8}
              value={modelAliases}
              onChange={e => setModelAliases(e.target.value)}
              className="code-surface flex min-h-[160px] w-full rounded-2xl px-3 py-2 text-sm font-mono"
              style={{ whiteSpace: "pre", overflowX: "auto" }}
            />
            <div className="mt-4 flex justify-end">
              <Button onClick={handleSaveAliases}>{t("settings.saveAliases")}</Button>
            </div>
          </div>
        </div>

        {/* Usage Example */}
        <div className="admin-card min-w-0 overflow-hidden">
          <div className="admin-card-header flex flex-col space-y-1.5">
            <div className="flex items-center gap-2">
              <Code className="h-5 w-5 text-primary" />
              <h3 className="font-semibold leading-none tracking-tight">{t("settings.examples")}</h3>
            </div>
          </div>
          <div className="p-6 min-w-0">
            <pre className="code-surface rounded-2xl p-4 text-xs font-mono whitespace-pre-wrap break-all max-h-[400px] overflow-y-auto overflow-x-hidden">
              {curlExample}
            </pre>
          </div>
        </div>
      </div>
    </div>
  )
}
