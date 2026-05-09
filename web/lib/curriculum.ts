// The locked curriculum from the plan. SessionNav and the landing page both
// read from this single source of truth. Slugs match docs/{zh,en}/<slug>.md.
//
// "available: false" means the chapter exists in the curriculum but its
// docs aren't written yet — the link will render but go to a placeholder.

export type ChapterMeta = {
  slug: string;
  num: string; // "s01", "s02", "s_full"
  title: { zh: string; en: string };
  available: boolean;
};

export const CURRICULUM: ChapterMeta[] = [
  {
    slug: "s01-minimal-loop",
    num: "s01",
    title: {
      zh: "最小 think→act→observe 循环",
      en: "Minimal think→act→observe loop",
    },
    available: true,
  },
  {
    slug: "s02-command-registry",
    num: "s02",
    title: { zh: "显式命令注册表", en: "Explicit command registry" },
    available: true,
  },
  {
    slug: "s03-llm-provider",
    num: "s03",
    title: { zh: "LLM Provider 多后端", en: "LLM provider with multiple backends" },
    available: true,
  },
  {
    slug: "s04-prompt-strategy",
    num: "s04",
    title: { zh: "Prompt 策略与解析", en: "Prompt strategies & response parsing" },
    available: false,
  },
  {
    slug: "s05-episodic-history",
    num: "s05",
    title: { zh: "情节式动作历史", en: "Episodic action history" },
    available: false,
  },
  {
    slug: "s06-workspace",
    num: "s06",
    title: { zh: "沙箱化 Workspace", en: "Sandboxed workspace storage" },
    available: false,
  },
  {
    slug: "s07-permissions",
    num: "s07",
    title: { zh: "分层权限管理", en: "Layered permission system" },
    available: false,
  },
  {
    slug: "s08-components",
    num: "s08",
    title: { zh: "可插拔 Component 系统", en: "Pluggable component system" },
    available: false,
  },
  {
    slug: "s09-continuous-mode",
    num: "s09",
    title: { zh: "持续运行模式与 UI", en: "Continuous mode & UI feedback" },
    available: false,
  },
  {
    slug: "s10-reflexion-hooks",
    num: "s10",
    title: { zh: "Reflexion 与 AfterParse hooks", en: "Reflexion & AfterParse pipeline" },
    available: false,
  },
  {
    slug: "s_full-integration",
    num: "s_full",
    title: { zh: "端到端集成", en: "End-to-end integration" },
    available: false,
  },
  {
    slug: "appendix-a-classic-vs-modern",
    num: "A",
    title: {
      zh: "附录 A · Classic vs 现代 Agent 架构",
      en: "Appendix A · Classic vs Modern agent architectures",
    },
    available: false,
  },
  {
    slug: "appendix-b-upstream-map",
    num: "B",
    title: {
      zh: "附录 B · 上游源码导读地图",
      en: "Appendix B · Upstream source-reading map",
    },
    available: false,
  },
];

export type Locale = "zh" | "en";

export function chapterTitle(c: ChapterMeta, locale: Locale): string {
  return c.title[locale];
}
