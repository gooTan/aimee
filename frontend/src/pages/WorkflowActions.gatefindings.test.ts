import { describe, expect, it } from "vitest";
import { parseGateFindings } from "./WorkflowActions";

describe("parseGateFindings", () => {
  it("fills location, summary, and recommendation from the left", () => {
    expect(
      parseGateFindings(
        "src/duration.js:42 | error drops the unit | include the unit\n" +
          "README.md | example shows the old CLI name\n" +
          "tests never assert exit codes\n" +
          "   \n",
      ),
    ).toEqual([
      { location: "src/duration.js:42", summary: "error drops the unit", recommendation: "include the unit" },
      { location: "README.md", summary: "example shows the old CLI name" },
      { summary: "tests never assert exit codes" },
    ]);
  });

  it("keeps extra pipes inside the recommendation", () => {
    expect(parseGateFindings("a.ts:1 | fix it | use x | not y")).toEqual([
      { location: "a.ts:1", summary: "fix it", recommendation: "use x | not y" },
    ]);
  });

  it("drops lines with no summary", () => {
    expect(parseGateFindings("loc |  \n|\n")).toEqual([]);
  });
});
