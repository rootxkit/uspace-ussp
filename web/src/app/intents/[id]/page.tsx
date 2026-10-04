import type { Metadata } from "next";
import { pageTitle } from "@/i18n/server";
import { IntentPage } from "@/portal/intents/IntentPage";

export async function generateMetadata(): Promise<Metadata> {
  return { title: await pageTitle("portal.title.intent") };
}

export default async function Page({ params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  return <IntentPage id={id} />;
}
