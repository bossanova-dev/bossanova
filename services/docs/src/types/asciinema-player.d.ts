declare module 'asciinema-player' {
  export type AsciinemaPlayerOptions = {
    autoPlay?: boolean;
    loop?: boolean;
    controls?: boolean;
    fit?: 'width' | 'height' | 'both' | false;
    cols?: number;
    rows?: number;
    speed?: number;
    idleTimeLimit?: number;
    terminalFontFamily?: string;
  };

  export type AsciinemaPlayerInstance = {
    dispose?: () => void;
  };

  export function create(
    src: string,
    element: HTMLElement,
    options?: AsciinemaPlayerOptions,
  ): AsciinemaPlayerInstance;
}
