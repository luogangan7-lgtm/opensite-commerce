/** Bootstraps the admin shell; depends on routing/query and shared CSS; called by index.html. Gallery is build-guarded. */
import React from 'react';
import ReactDOM from 'react-dom/client';
import { BrowserRouter } from 'react-router';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import '@fontsource/inter/400.css';
import '@fontsource/inter/500.css';
import '@fontsource/inter/600.css';
import '@opensite/design-tokens/css';
import '@opensite/ui/css';
import './styles.css';
import { App } from './app';
import { t } from '@opensite/component-contracts';

document.title = t('app.ready');

const client = new QueryClient();
ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode><QueryClientProvider client={client}><BrowserRouter><App /></BrowserRouter></QueryClientProvider></React.StrictMode>,
);
