import { useId } from "react"

/** Original, asset-free orbital sculpture. Decorative only: never a health indicator. */
export function GatewayArt({ className = "" }: { className?: string }) {
  const id = useId().replace(/:/g, "")
  return (
    <div className={`gateway-art ${className}`} aria-hidden="true">
      <svg viewBox="0 0 640 520" fill="none">
        <defs>
          <radialGradient id={`${id}-halo`}>
            <stop stopColor="#338aff" stopOpacity=".32" />
            <stop offset="1" stopColor="#338aff" stopOpacity="0" />
          </radialGradient>
          <linearGradient id={`${id}-metal`} x1="210" y1="120" x2="440" y2="400" gradientUnits="userSpaceOnUse">
            <stop stopColor="#efffff" />
            <stop offset=".2" stopColor="#b7eeff" />
            <stop offset=".38" stopColor="#357daf" />
            <stop offset=".5" stopColor="#092b52" />
            <stop offset=".62" stopColor="#8cdfff" />
            <stop offset=".76" stopColor="#e0fbff" />
            <stop offset="1" stopColor="#30719c" />
          </linearGradient>
          <linearGradient id={`${id}-ring`} x1="110" y1="140" x2="540" y2="400" gradientUnits="userSpaceOnUse">
            <stop stopColor="#b7f7ff" stopOpacity=".12" />
            <stop offset=".45" stopColor="#c6faff" />
            <stop offset="1" stopColor="#338aff" stopOpacity=".18" />
          </linearGradient>
        </defs>
        <ellipse cx="320" cy="260" rx="310" ry="250" fill={`url(#${id}-halo)`} />
        <g className="gateway-art-orbits">
          <ellipse cx="320" cy="260" rx="276" ry="96" transform="rotate(-28 320 260)" stroke={`url(#${id}-ring)`} strokeWidth="1.5" />
          <ellipse cx="320" cy="260" rx="256" ry="126" transform="rotate(43 320 260)" stroke="#8edbff" strokeOpacity=".22" />
          <ellipse cx="320" cy="260" rx="196" ry="220" transform="rotate(30 320 260)" stroke="#8edbff" strokeOpacity=".12" strokeDasharray="3 9" />
          <circle cx="91" cy="355" r="5" fill="#d4fa55" />
          <circle cx="540" cy="354" r="3" fill="#a4f6ff" />
        </g>
        <g className="gateway-art-core">
          <path d="M319 107 446 180 446 328 319 403 192 329 192 182Z" fill={`url(#${id}-metal)`} stroke="#d3f8ff" strokeOpacity=".7" />
          <path d="m319 107 1 147 126-74M320 254l-128-72m128 72-1 149m1-149 126 74m-126-74-128 75" stroke="#d4f8ff" strokeOpacity=".42" />
          <path d="m319 174 69 40v80l-69 40-68-40v-80Z" fill="#071a2e" fillOpacity=".78" stroke="#bcefff" strokeOpacity=".72" />
          <path d="M293 286v-63h49m-49 27h37" stroke="#d3fbff" strokeWidth="7" strokeLinecap="square" />
          <path d="m319 121 113 66" stroke="white" strokeOpacity=".65" />
        </g>
        <path d="M58 420h71m-35-6v12M509 90h71m-35-6v12" stroke="#9cdfff" strokeOpacity=".35" />
      </svg>
    </div>
  )
}
