// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render } from "@testing-library/react";
import { boot, STORAGE } from "../../i18n";
import type { StateTool } from "../../state/outputcap";
import { ToolCard } from "./ToolCard";

function setLang(lang: "zh" | "en") {
  localStorage.setItem(STORAGE, lang);
  boot();
}

afterEach(() => {
  cleanup();
  setLang("zh");
});

const shell = (name: string): StateTool => ({
  id: "b1",
  name: "bash",
  args: '{"command":"echo ok"}',
  readOnly: false,
  execution: { kind: "shell", shell: name },
});

// The tag carries the id; the tooltip says which interpreter took the command.
// It is a table value behind t(), so neither catalogue scan reaches it.
describe("the shell tag's tooltip", () => {
  it("names the interpreter in the window's language", () => {
    setLang("zh");
    const zh = render(<ToolCard activity tool={shell("bash")} running={false} />);
    expect(zh.container.querySelector(".tag")?.getAttribute("title")).toBe("命令交给 bash 执行");

    cleanup();
    setLang("en");
    const en = render(<ToolCard activity tool={shell("powershell")} running={false} />);
    expect(en.container.querySelector(".tag")?.getAttribute("title")).toBe(
      "No bash on this machine; the command is handed to Windows PowerShell — it does not support && or ||",
    );
  });
});
