import type { Metadata } from "next";
import localFont from "next/font/local";
import "./globals.css";

// Noto Sans Georgian, bundled from the pinned @fontsource package and
// served by this app: no third-party font request (CLAUDE.md, M38).
const notoSansGeorgian = localFont({
  src: [
    { path: "../../node_modules/@fontsource/noto-sans-georgian/files/noto-sans-georgian-georgian-400-normal.woff2", weight: "400" },
    { path: "../../node_modules/@fontsource/noto-sans-georgian/files/noto-sans-georgian-georgian-700-normal.woff2", weight: "700" },
    { path: "../../node_modules/@fontsource/noto-sans-georgian/files/noto-sans-georgian-latin-400-normal.woff2", weight: "400" },
    { path: "../../node_modules/@fontsource/noto-sans-georgian/files/noto-sans-georgian-latin-700-normal.woff2", weight: "700" },
  ],
  variable: "--font-noto-sans-georgian",
  display: "swap",
});

export const metadata: Metadata = {
  title: "uspace USSP",
};

export default function RootLayout({ children }: LayoutProps<"/">) {
  return (
    <html lang="ka" className={`${notoSansGeorgian.variable} h-full antialiased`}>
      <body className="min-h-full flex flex-col">{children}</body>
    </html>
  );
}
