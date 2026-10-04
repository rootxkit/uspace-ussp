import type { Metadata } from "next";
import { pageTitle } from "@/i18n/server";
import { RegisterPage } from "@/portal/RegisterPage";

export async function generateMetadata(): Promise<Metadata> {
  return { title: await pageTitle("portal.title.register") };
}

export default function Page() {
  return <RegisterPage />;
}
