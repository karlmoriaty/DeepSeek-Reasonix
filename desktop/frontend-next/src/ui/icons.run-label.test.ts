// @vitest-environment jsdom
//
// jsdom only because boot() reads the stored language; the labels are pure
// functions of the category and the installed catalogue.
import { afterEach, describe, expect, it } from "vitest";
import { boot, STORAGE } from "../i18n";
import { runLabelFor } from "./icons";

function setLang(lang: "zh" | "en") {
  localStorage.setItem(STORAGE, lang);
  boot();
}

afterEach(() => setLang("zh"));

const RUNNING: Array<[string, string, string]> = [
  ["todo_write", "正在写计划…", "Planning…"],
  ["web_search", "正在联网…", "Using the network…"],
  ["task", "正在派活…", "Delegating…"],
  ["bash", "正在执行…", "Running…"],
  ["edit_file", "正在改写…", "Writing…"],
  ["remember", "正在写入记忆…", "Saving to memory…"],
  ["use_capability", "正在调 MCP…", "Calling MCP…"],
];

// The line a running tool draws is passed through t() as a table value, so the
// catalogue test cannot see it and a missing key renders as Chinese in an
// English window.
describe("a running tool names what it is doing in the window's language", () => {
  it("translates every category that has its own running line", () => {
    for (const [tool, zh, en] of RUNNING) {
      setLang("zh");
      expect(runLabelFor(tool), tool).toBe(zh);
      setLang("en");
      expect(runLabelFor(tool), tool).toBe(en);
    }
  });

  it("falls back to the generic line in both languages", () => {
    setLang("zh");
    expect(runLabelFor("some_unlisted_tool")).toBe("正在处理…");
    setLang("en");
    expect(runLabelFor("some_unlisted_tool")).toBe("Working…");
  });
});
