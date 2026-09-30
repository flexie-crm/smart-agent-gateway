import { describe, expect, it } from "vitest";
import { describeFile } from "@/lib/filekind";

describe("naming a file's kind", () => {
  it("names the four runtimes a skill's scripts use", () => {
    expect(describeFile("scripts/to_excel.py", "text/x-python")).toBe("Python");
    expect(describeFile("scripts/summary.js", "text/javascript; charset=utf-8"))
      .toBe("JavaScript");
    expect(describeFile("scripts/run.sh", "text/x-shellscript")).toBe("Shell");
    expect(describeFile("scripts/run.ps1", "text/plain")).toBe("PowerShell");
  });

  it("reads the extension, not the mime type", () => {
    // The product already decides by extension everywhere else: which runtime
    // runs a script, which grammar colours it. A second opinion off the mime
    // type would be a second thing that can disagree, and a package can carry
    // a file whose mime type the store guessed.
    expect(describeFile("scripts/summary.mjs", "application/octet-stream")).toBe(
      "JavaScript",
    );
  });

  it("falls back to the mime type without its parameters", () => {
    expect(describeFile("LICENSE", "text/plain; charset=utf-8")).toBe("plain");
    expect(describeFile("notes", "text/x-rustsrc")).toBe("x-rustsrc");
  });

  it("calls unrecognised bytes what they are", () => {
    expect(describeFile("assets/blob", "application/octet-stream")).toBe("Binary");
    expect(describeFile("assets/blob", "")).toBe("Binary");
  });

  it("names what a package carries", () => {
    expect(
      describeFile(
        "assets/template.docx",
        "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
      ),
    ).toBe("Word document");
    expect(describeFile("assets/sample-sales.csv", "text/csv")).toBe("CSV");
  });
});
