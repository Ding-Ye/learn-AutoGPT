import Link from "next/link";
import { notFound } from "next/navigation";
import { CURRICULUM, chapterTitle, type Locale } from "@/lib/curriculum";

export default async function Landing({
  params,
}: {
  params: Promise<{ locale: string }>;
}) {
  const { locale } = await params;
  if (locale !== "zh" && locale !== "en") notFound();
  const l = locale as Locale;

  const intro = l === "zh" ? INTRO_ZH : INTRO_EN;
  const ctaLabel = l === "zh" ? "从 s01 开始 →" : "Start at s01 →";

  return (
    <article className="prose-doc">
      <h1>learn-AutoGPT</h1>
      <p className="text-[var(--fg-muted)]">
        {l === "zh"
          ? "用 Go 从零渐进构建一个 AutoGPT classic 风格的 autonomous agent，每节末尾对照上游 Python 源码。"
          : "Build an AutoGPT-classic-style autonomous agent from scratch in Go, session by session — each chapter ends with the upstream Python source."}
      </p>

      {intro.map((p, i) => (
        <p key={i}>{p}</p>
      ))}

      <p>
        <Link
          href={`/${l}/s/s01-minimal-loop`}
          className="inline-block mt-2 px-4 py-2 rounded border border-[var(--accent-soft)] hover:border-[var(--accent)]"
        >
          {ctaLabel}
        </Link>
      </p>

      <h2>{l === "zh" ? "课程" : "Curriculum"}</h2>
      <ul>
        {CURRICULUM.map((c) => (
          <li key={c.slug}>
            <span className="font-mono text-[var(--fg-muted)] mr-2">
              {c.num}
            </span>
            {c.available ? (
              <Link href={`/${l}/s/${c.slug}`}>{chapterTitle(c, l)}</Link>
            ) : (
              <span className="text-[var(--fg-muted)]">
                {chapterTitle(c, l)}{" "}
                <span className="text-xs">
                  ({l === "zh" ? "未发布" : "not yet"})
                </span>
              </span>
            )}
          </li>
        ))}
      </ul>
    </article>
  );
}

const INTRO_ZH = [
  "这个仓库的目标不是教你「用」 AutoGPT，而是讲清楚它的核心机制是怎么从零长出来的。聚焦上游的 classic/ 子目录（原始 AutoGPT agent，MIT 许可），不动 autogpt_platform/。",
  "每一节加一个机制——think→act→observe loop、命令注册、LLM Provider 抽象、Prompt 策略、情节式历史、Workspace 沙箱、分层权限、Component 系统、持续运行、Reflexion + AfterParse hooks——用 Go 写一份精简实现。看完十节，AutoGPT 不再是一团黑魔法。",
  "Go 实现是教学骨架，AutoGPT classic 上游是 Python 实现。每节末尾的「上游源码阅读」把这两边对照起来，你能从 mini 版顺着指针读到生产代码。",
];

const INTRO_EN = [
  "The goal of this repo is not to teach you to *use* AutoGPT — it is to teach you how its core mechanisms grow from scratch. We focus on the upstream `classic/` subtree (the original AutoGPT agent, MIT-licensed) and leave `autogpt_platform/` (Polyform Shield) alone.",
  "Each chapter adds one mechanism — think→act→observe loop, command registry, LLM provider abstraction, prompt strategies, episodic history, workspace sandbox, layered permissions, component system, continuous mode, Reflexion + AfterParse hooks — implemented as a small Go file. After ten chapters, AutoGPT stops being black magic.",
  "Go is the teaching skeleton; the upstream Python is the production implementation. The 'Upstream Source Reading' section at the end of every chapter bridges them — you can follow the pointers from the mini version straight into the real code.",
];
