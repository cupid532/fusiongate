import { render } from "@testing-library/react"
import { describe, expect, it } from "vitest"
import { Heatmap } from "./heatmap"

describe("Heatmap", () => {
  it("passes exact row and column indices to duplicate-label tooltips", () => {
    const { container } = render(
      <Heatmap
        matrix={[
          [10, 20],
          [30, 40],
        ]}
        rowLabels={["shared-model", "second-model"]}
        colLabels={["01", "01"]}
        formatTooltip={(row, col, value, rowIndex, colIndex) => `${row}/${col}/${value}/${rowIndex}/${colIndex}`}
      />,
    )

    const cells = Array.from(container.querySelectorAll<HTMLElement>("[title]"))
    expect(cells.map((cell) => cell.title)).toEqual([
      "shared-model/01/10/0/0",
      "shared-model/01/20/0/1",
      "second-model/01/30/1/0",
      "second-model/01/40/1/1",
    ])
  })
})
