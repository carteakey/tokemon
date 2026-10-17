import type { CSSProperties, ReactNode } from "react";

// A CSS-only rolling odometer. The lowest five columns animate with
// hold-then-slide keyframes (odo0..odo4 in globals.css) whose periods are
// powers of ten of one tick, so higher columns turn over exactly when the
// column below wraps. Negative animation delays start the roll at `value`.
// With reduced motion, each column rests on its starting digit.
const TICK_SECONDS = 0.3;
const ROLLING_COLUMNS = 5;
const STRIP = ["0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "0"];

export function Odometer({ value, label }: { value: number; label: string }) {
  const text = value.toString();
  const formatted = value.toLocaleString("en-US");
  const columns = text.length;
  const cells: ReactNode[] = [];

  text.split("").forEach((char, index) => {
    const column = columns - 1 - index; // 0 = ones
    const digit = Number(char);
    const rolling = column < ROLLING_COLUMNS;
    const delay = rolling ? -((value % 10 ** (column + 1)) * TICK_SECONDS) : 0;
    const style = { "--d": digit, animationDelay: rolling ? `${delay.toFixed(2)}s` : undefined } as CSSProperties;
    cells.push(
      <span className="odoCell" key={`d${index}`}>
        <span className={rolling ? `odoStrip odoRoll${column}` : "odoStrip"} style={style}>
          {STRIP.map((glyph, i) => <span key={i}>{glyph}</span>)}
        </span>
      </span>,
    );
    if (column > 0 && column % 3 === 0) cells.push(<span className="odoComma" key={`c${index}`}>,</span>);
  });

  return (
    <div className="odometer" role="img" aria-label={`${label}: ${formatted}, counting up`}>
      {cells}
    </div>
  );
}
