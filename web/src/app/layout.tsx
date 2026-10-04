import type { ReactNode } from "react";
import type { Metadata } from "next";
import { cookies, headers } from "next/headers";
import { CSP_NONCE_HEADER, readSessionToken, sessionDisplay } from "@rootxkit/uspace-ui/auth/server";
import { fontClassName } from "@rootxkit/uspace-ui/fonts";
import { LANG_COOKIE, negotiateLang } from "@rootxkit/uspace-ui/i18n";
import { brandFromEnv } from "@rootxkit/uspace-ui/theme";
import { Providers } from "@/components/Providers";
import { Shell } from "@/components/Shell";
import { runtimeConfig } from "@/config";
import { pageTitle } from "@/i18n/server";
import "./globals.css";

// Every page reads the request: the language, the session, the nonce.
export const dynamic = "force-dynamic";

export async function generateMetadata(): Promise<Metadata> {
  return { title: await pageTitle("portal.title.home") };
}

// The portal's frame (brief WP-17): the kit's theme, fonts (Noto Sans
// and Noto Sans Georgian through next/font/local), language (the
// uspace_lang cookie, then Accept-Language, then ka) and session display
// (decoded unverified, display only: the API decides every request). The
// traffic and alert WebSockets are opened by the pages on this origin
// with the session cookie; never a ticket (M22).
export default async function RootLayout({ children }: { children: ReactNode }) {
  const jar = await cookies();
  const h = await headers();
  const lang = negotiateLang(h.get("accept-language"), jar.get(LANG_COOKIE)?.value ?? null);
  const session = sessionDisplay(readSessionToken(jar));
  const nonce = h.get(CSP_NONCE_HEADER) ?? undefined;
  const brand = brandFromEnv(process.env);
  return (
    <html lang={lang} className={fontClassName} suppressHydrationWarning>
      <body className="font-sans antialiased">
        <Providers lang={lang} brand={brand} nonce={nonce} session={session} config={runtimeConfig()}>
          <Shell>{children}</Shell>
        </Providers>
      </body>
    </html>
  );
}
