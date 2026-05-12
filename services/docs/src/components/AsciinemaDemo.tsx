import React, { useEffect, useRef } from 'react';
import 'asciinema-player/dist/bundle/asciinema-player.css';

type AsciinemaDemoProps = {
  src: string;
  cols?: number;
  rows?: number;
  speed?: number;
  idleTimeLimit?: number;
};

type AsciinemaPlayerModule = {
  create: (
    src: string,
    element: HTMLElement,
    options: Record<string, unknown>,
  ) => { dispose?: () => void };
};

export default function AsciinemaDemo({
  src,
  cols,
  rows,
  speed = 1.2,
  idleTimeLimit = 1,
}: AsciinemaDemoProps): React.ReactElement {
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    let cancelled = false;
    let player: { dispose?: () => void } | undefined;

    async function mount() {
      const el = ref.current;
      if (!el) return;

      const AsciinemaPlayer = (await import('asciinema-player')) as AsciinemaPlayerModule;

      if (cancelled) return;

      player = AsciinemaPlayer.create(src, el, {
        autoPlay: true,
        loop: true,
        controls: false,
        fit: 'width',
        cols,
        rows,
        speed,
        idleTimeLimit,
        terminalFontFamily:
          '"JetBrains Mono", "SF Mono", ui-monospace, "Fira Code", Menlo, Consolas, monospace',
      });
    }

    mount();

    return () => {
      cancelled = true;
      player?.dispose?.();
    };
  }, [cols, idleTimeLimit, rows, speed, src]);

  return <div className="asciinemaDemo" ref={ref} />;
}
