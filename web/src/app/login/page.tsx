import type { Metadata } from "next";
import { pageTitle } from "@/i18n/server";
import { LoginPage } from "@/portal/LoginPage";

export async function generateMetadata(): Promise<Metadata> {
  return { title: await pageTitle("portal.title.login") };
}

export default function Page() {
  return <LoginPage />;
}
