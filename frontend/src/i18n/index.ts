import i18n from "i18next"
import { initReactI18next } from "react-i18next"
import en from "./locales/en.json"

/**
 * Languages the UI ships with. To add one: create `locales/<code>.json` with the
 * same keys as `en.json`, import it here, and add it to `resources` and
 * `SUPPORTED_LANGUAGES`. The selected language is remembered in localStorage.
 */
export const SUPPORTED_LANGUAGES = [{ code: "en", label: "English" }] as const

export const DEFAULT_LANGUAGE = "en"
const STORAGE_KEY = "qwen2api_lang"

function detectLanguage(): string {
  const supported = SUPPORTED_LANGUAGES.map(l => l.code as string)
  try {
    const stored = localStorage.getItem(STORAGE_KEY)
    if (stored && supported.includes(stored)) return stored
  } catch {
    // localStorage may be unavailable (private mode); fall back to the default.
  }
  const browser = typeof navigator !== "undefined" ? navigator.language?.split("-")[0] : ""
  return browser && supported.includes(browser) ? browser : DEFAULT_LANGUAGE
}

export function setLanguage(code: string) {
  try {
    localStorage.setItem(STORAGE_KEY, code)
  } catch {
    // Non-persistent language switch is fine.
  }
  void i18n.changeLanguage(code)
}

void i18n.use(initReactI18next).init({
  resources: { en: { translation: en } },
  lng: detectLanguage(),
  fallbackLng: DEFAULT_LANGUAGE,
  supportedLngs: SUPPORTED_LANGUAGES.map(l => l.code),
  interpolation: { escapeValue: false },
  returnNull: false,
})

if (typeof document !== "undefined") {
  document.documentElement.lang = i18n.language
  i18n.on("languageChanged", lng => {
    document.documentElement.lang = lng
  })
}

export default i18n
