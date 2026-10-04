import type { Metadata } from "next";
import { pageTitle } from "@/i18n/server";
import { ConsoleLoginPage } from "@/console/LoginPage";

export async function generateMetadata(): Promise<Metadata> {
  return { title: await pageTitle("console.title.login") };
}

export default function Page() {
  return <ConsoleLoginPage />;
}
